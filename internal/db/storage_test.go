package db_test

import (
	"context"
	"slices"
	"testing"

	"github.com/wakeshift/server/internal/dbtest"
)

// The hot tables carry their storage settings (163_hot_table_storage.sql),
// and the unused events_type index is gone (164_drop_events_type.sql).
func TestHotTableStorage(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	want := map[string][]string{
		"grants":               {"fillfactor=70", "autovacuum_vacuum_scale_factor=0", "autovacuum_vacuum_threshold=500"},
		"subscription_cursors": {"fillfactor=70", "autovacuum_vacuum_scale_factor=0", "autovacuum_vacuum_threshold=1000"},
		"event_counters":       {"fillfactor=70", "autovacuum_vacuum_scale_factor=0", "autovacuum_vacuum_threshold=500"},
		"mcp_event_webhooks":   {"fillfactor=70", "autovacuum_vacuum_scale_factor=0", "autovacuum_vacuum_threshold=500"},
		"pipeline_due":         {"fillfactor=70", "autovacuum_vacuum_scale_factor=0", "autovacuum_vacuum_threshold=500"},
		"deliveries":           {"fillfactor=85", "autovacuum_vacuum_scale_factor=0.02", "autovacuum_analyze_scale_factor=0.02"},
	}
	for table, opts := range want {
		var got []string
		if err := d.Pool.QueryRow(ctx, `SELECT coalesce(reloptions, '{}') FROM pg_class WHERE oid = $1::regclass`, table).Scan(&got); err != nil {
			t.Fatal(err)
		}
		for _, o := range opts {
			if !slices.Contains(got, o) {
				t.Errorf("%s: options %v, missing %s", table, got, o)
			}
		}
	}
	var exists bool
	if err := d.Pool.QueryRow(ctx, `SELECT to_regclass('events_type') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatalf("events_type exists %v %v", exists, err)
	}
}
