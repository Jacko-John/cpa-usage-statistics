package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Jacko-John/cpa-usage-statistics/internal/storage"
)

func TestNotificationSchemaDoesNotChangeUsageRecords(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy=%v", legacy), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "usage.db")
			if legacy {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(storage.CreateUsageRecordsSQL); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`INSERT INTO usage_records(id,timestamp) VALUES('legacy',?)`, time.Now().Unix()); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			s, err := openTestStore(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			columns, err := s.ExistingColumns(context.Background())
			if err != nil || len(columns) != 25 {
				t.Fatal(columns, err)
			}
			for name := range columns {
				if strings.HasPrefix(name, "notification_") {
					t.Fatal("notification column added to usage_records", name)
				}
			}
			var count int
			if err := s.DB.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('notification_basis','notification_progress','idx_notification_pending')`).Scan(&count); err != nil || count != 0 {
				t.Fatal("redundant notification schema", count, err)
			}
			if legacy {
				if err := s.DB.QueryRow(`SELECT COUNT(*) FROM usage_records WHERE id='legacy'`).Scan(&count); err != nil || count != 1 {
					t.Fatal("legacy record lost", count, err)
				}
			}
		})
	}
}
