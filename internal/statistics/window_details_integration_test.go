package statistics_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	usagemodel "github.com/Jacko-John/cpa-usage-statistics/internal/model"
	"github.com/Jacko-John/cpa-usage-statistics/internal/statistics"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testfixture"
	"github.com/Jacko-John/cpa-usage-statistics/internal/testutil"
)

func TestUsageAPIWindowDetailsCachedAndHistorical(t *testing.T) {
	for _, historical := range []bool{false, true} {
		t.Run(fmt.Sprintf("historical=%v", historical), func(t *testing.T) {
			s := testfixture.New(t)
			ctx := context.Background()
			now := statistics.FloorUnix(time.Now().Unix(), 60)
			start := now - 120
			if historical {
				start = now - 70*86400
			}
			pricing := statistics.Pricing{
				Prices: map[string]statistics.ModelPrice{
					"model": {Input: 2, Output: 4, ContextTiers: []statistics.ContextTierPrice{{Threshold: 1_000_000, Input: 3, Output: 6}}},
					"free":  {},
				},
				TierMultipliers: []statistics.TierMultiplier{{Model: "model", Tier: "priority", Multiplier: 2}, {Model: "model", Tier: "flex", Multiplier: 0.5}},
			}
			if _, err := s.UpdatePricing(ctx, pricing); err != nil {
				t.Fatal(err)
			}
			for i, input := range []struct {
				model, tier string
				in, out     int64
			}{
				{"model", "PRIORITY", 1_000_000, 100_000},
				{"model", "priority", 1_000_000, 100_000},
				{"model", "priority", 2_000_000, 100_000},
				{"model", "default", 1_000_000, 0},
				{"model", "flex", 1_000_000, 0},
				{"model", "", 1_000_000, 0},
				{"unknown", "default", 1_000_000, 0},
				{"free", "", 1_000_000, 0},
			} {
				if err := s.Insert(ctx, usagemodel.Record{
					ID: fmt.Sprintf("detail-%d", i), APIKey: "key", Model: input.model, ServiceTier: input.tier,
					Timestamp: time.Unix(start+int64(i)+1, 0), Tokens: usagemodel.TokenStats{InputTokens: input.in, OutputTokens: input.out},
				}); err != nil {
					t.Fatal(err)
				}
			}
			want := []statistics.APIWindowDetail{
				{Model: "free", Requests: 1, Tokens: 1_000_000},
				{Model: "model", Requests: 1, Tokens: 1_000_000, Cost: 2},
				{Model: "model", ServiceTier: "default", Requests: 1, Tokens: 1_000_000, Cost: 2},
				{Model: "model", ServiceTier: "flex", Requests: 1, Tokens: 1_000_000, Cost: 1},
				{Model: "model", ServiceTier: "priority", Requests: 3, Tokens: 4_300_000, Cost: 22.8},
				{Model: "unknown", ServiceTier: "default", Requests: 1, Tokens: 1_000_000, Unpriced: 1},
			}
			check := func(requests, tokens int64, cost float64) {
				t.Helper()
				if err := s.WithConn(ctx, func(conn *sql.Conn) error {
					q := &windowQueryRecorder{}
					if historical {
						q.SQLQueryer = conn
					}
					got, err := s.UsageAPIWindows(ctx, q, []statistics.APIWindowQuery{
						{APIKey: "key", Start: start, End: start + 60},
						{APIKey: "missing", Start: start, End: start + 60},
					}, now)
					if err != nil {
						return err
					}
					wantQueries := 0
					if historical {
						wantQueries = 1
					}
					if q.calls != wantQueries || got[0].Requests != requests || got[0].Tokens != tokens || got[0].Unpriced != 1 {
						t.Fatalf("summary=%+v queries=%d", got, q.calls)
					}
					testutil.AssertCost(t, got[0].Cost, cost)
					if len(got[0].Details) != len(want) {
						t.Fatalf("details=%+v want=%+v", got[0].Details, want)
					}
					for i, detail := range got[0].Details {
						testutil.AssertCost(t, detail.Cost, want[i].Cost)
						detail.Cost = want[i].Cost
						if !reflect.DeepEqual(detail, want[i]) {
							t.Fatalf("detail %d: got=%+v want=%+v", i, detail, want[i])
						}
					}
					if got[1].Details == nil || len(got[1].Details) != 0 {
						t.Fatal("known empty usage must have an empty breakdown", got[1])
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			check(8, 9_300_000, 27.8)
			ids := usagemodel.NewStableStringSet(1)
			ids.Add("detail-0")
			if result, err := s.Delete(ctx, ids); err != nil || result.Deleted != 1 {
				t.Fatal(result, err)
			}
			want[4].Requests, want[4].Tokens, want[4].Cost = 2, 3_200_000, 18
			check(7, 8_200_000, 23)
			pricing.Prices["model"] = statistics.ModelPrice{Input: 4, Output: 8, ContextTiers: []statistics.ContextTierPrice{{Threshold: 1_000_000, Input: 6, Output: 12}}}
			if _, err := s.UpdatePricing(ctx, pricing); err != nil {
				t.Fatal(err)
			}
			for i := range want {
				want[i].Cost *= 2
			}
			check(7, 8_200_000, 46)
		})
	}
}
