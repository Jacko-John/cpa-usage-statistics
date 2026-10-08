package notifications

import (
	"context"
	"database/sql"
	"html"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func TestNotificationDetailsFreezeAndRestart(t *testing.T) {
	ctx := context.Background()
	base := statistics.FloorUnix(time.Now().Unix()-86400, 86400) + 19*3600 + 58*60
	s, id := notificationTestStore(t, base)
	setPrice := func(price float64) {
		t.Helper()
		if err := s.Statistics.Rebuild(ctx, s.DB, statistics.Pricing{
			Prices:          map[string]statistics.ModelPrice{"model": {Input: price}},
			TierMultipliers: []statistics.TierMultiplier{{Model: "model", Tier: "priority", Multiplier: 2}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setPrice(1)
	for i, tier := range []string{"default", "priority", ""} {
		if err := s.Insert(ctx, usagemodel.Record{
			ID: "detail-" + tier, APIKey: "test-client-key", Model: "model", ServiceTier: tier,
			Timestamp: time.Unix(base+60+int64(i), 0), Tokens: usagemodel.TokenStats{InputTokens: 1_000_000},
		}); err != nil {
			t.Fatal(err)
		}
	}
	history, err := s.HistoryGet(ctx, id, base+90)
	if err != nil || len(history.Current) != 1 || len(history.Current[0].Details) != 3 || history.Current[0].Cost != 4 {
		t.Fatalf("current=%+v err=%v", history, err)
	}
	setPrice(2)
	// 封存失败时，明细、金额和游标必须一起回滚，下次仍能重试。
	if _, err := s.DB.Exec(`CREATE TRIGGER fail_details BEFORE UPDATE OF details ON notification_periods BEGIN SELECT RAISE(ABORT,'injected snapshot failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.HistoryGet(ctx, id, base+120); err == nil {
		t.Fatal("expected snapshot failure")
	}
	var cursor int64
	var encoded []byte
	if err := s.DB.QueryRow(`SELECT cursor FROM notification_segments WHERE subscription=?`, id).Scan(&cursor); err != nil || cursor >= base+120 {
		t.Fatal("cursor advanced after rollback", cursor, err)
	}
	if err := s.DB.QueryRow(`SELECT details FROM notification_periods WHERE subscription=? AND status='open'`, id).Scan(&encoded); err != nil || encoded != nil {
		t.Fatal("partial snapshot persisted", encoded, err)
	}
	if _, err := s.DB.Exec(`DROP TRIGGER fail_details`); err != nil {
		t.Fatal(err)
	}
	history, err = s.HistoryGet(ctx, id, base+120)
	if err != nil || len(history.Periods) != 1 || history.Periods[0].Cost != 8 {
		t.Fatalf("sealed=%+v err=%v", history, err)
	}
	frozen := history.Periods[0]
	want := []statistics.APIWindowDetail{
		{Model: "model", Requests: 1, Tokens: 1_000_000, Cost: 2},
		{Model: "model", ServiceTier: "default", Requests: 1, Tokens: 1_000_000, Cost: 2},
		{Model: "model", ServiceTier: "priority", Requests: 1, Tokens: 1_000_000, Cost: 4},
	}
	if !reflect.DeepEqual(frozen.Details, want) || history.Current[0].Details == nil || len(history.Current[0].Details) != 0 {
		t.Fatalf("details=%+v current=%+v", frozen.Details, history.Current)
	}
	setPrice(99)
	if _, err := s.DeleteBefore(ctx, time.Unix(base+120, 0)); err != nil {
		t.Fatal(err)
	}
	path := s.Path
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTestStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	history, err = reopened.HistoryGet(ctx, id, base+130)
	if err != nil || len(history.Periods) != 1 || !reflect.DeepEqual(history.Periods[0], frozen) {
		t.Fatalf("snapshot changed after repricing, deletion or restart: %+v err=%v", history, err)
	}
	var sent []Period
	reopened.send = func(_ context.Context, _ Settings, mail Mail) (bool, error) {
		sent = append(sent, mail.Periods...)
		return false, nil
	}
	if err := reopened.SendBatch(ctx); err != nil || len(sent) != 1 || !reflect.DeepEqual(sent[0].Details, frozen.Details) || sent[0].Cost != frozen.Cost {
		t.Fatalf("outbox changed breakdown: %+v err=%v", sent, err)
	}
}

func TestNotificationDetailsLegacyMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.Replace(notificationSchema, ", details BLOB", "", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO notification_periods(id,segment,subscription,start,end,timezone,email,key_label,note,partial,status,requests,tokens,cost)
 VALUES('legacy',1,'subscription',1,2,'UTC','user@example.com','masked','legacy',0,'sent',3,123,4.5)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		s, err := openTestStore(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.WithConn(context.Background(), func(conn *sql.Conn) error {
			periods, err := scanNotificationPeriods(context.Background(), conn, `WHERE id='legacy'`)
			if err != nil {
				return err
			}
			if len(periods) != 1 || periods[0].Details != nil || periods[0].Cost != 4.5 || periods[0].Requests != 3 || periods[0].Tokens != 123 {
				t.Fatalf("legacy history changed: %+v", periods)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNotificationMessageUsageDetails(t *testing.T) {
	mail := notificationSampleMailForTest()
	model := `model<script>alert("x")</script>`
	tier := `priority<&>`
	mail.Periods[0].Details = []statistics.APIWindowDetail{
		{Model: model, ServiceTier: tier, Requests: 2, Tokens: 1234, Cost: 0.000123},
		{Model: "free", Requests: 1},
		{Model: "unpriced", ServiceTier: "default", Requests: 2, Unpriced: 2},
	}
	_, parts := testutil.MessageParts(t, Message(mail, "sender@example.com"))
	for kind, body := range parts {
		for _, text := range []string{"使用明细", "Service tier", "$0.000123", "$0.000000", "1,234", "未提供", "暂无法估算", "2 次请求未计价"} {
			if !strings.Contains(body, text) {
				t.Errorf("%s missing detail %q", kind, text)
			}
		}
	}
	if !strings.Contains(parts["text/plain"], model) || !strings.Contains(parts["text/plain"], tier) {
		t.Fatal("plain text escaped details")
	}
	if strings.Contains(parts["text/html"], "<script>") || !strings.Contains(parts["text/html"], html.EscapeString(model)) || !strings.Contains(parts["text/html"], html.EscapeString(tier)) {
		t.Fatal("HTML details are not safely escaped")
	}
	for _, test := range []struct {
		details []statistics.APIWindowDetail
		message string
	}{
		{nil, "此历史周期未保存使用明细"},
		{[]statistics.APIWindowDetail{}, "本期没有使用记录"},
	} {
		mail.Periods[0].Details = test.details
		_, parts := testutil.MessageParts(t, Message(mail, "sender@example.com"))
		for _, body := range parts {
			if !strings.Contains(body, test.message) {
				t.Fatal("missing detail availability notice", test.message)
			}
		}
	}
}
