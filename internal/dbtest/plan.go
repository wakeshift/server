package dbtest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// IndexConds plans query in tx (EXPLAIN, not run) with sequential scans
// off, so a test's small tables plan as large ones would, and returns the
// index condition of each scan of relation ("" for a scan with none), and
// the plan.
func IndexConds(t testing.TB, tx pgx.Tx, relation, query string, args ...any) ([]string, []byte) {
	t.Helper()
	ctx := context.Background()
	noSeqScan(t, tx)
	var plan []byte
	if err := tx.QueryRow(ctx, `EXPLAIN (FORMAT JSON) `+query, args...).Scan(&plan); err != nil {
		t.Fatalf("explain: %v", err)
	}
	return scans(t, relation, plan), plan
}

// GenericIndexConds is IndexConds for the generic plan (EXPLAIN
// (GENERIC_PLAN)): the plan Postgres may cache for a prepared statement,
// as pgx's are, made without knowing the parameters' values. query keeps
// its placeholders; nothing is bound.
func GenericIndexConds(t testing.TB, tx pgx.Tx, relation, query string) ([]string, []byte) {
	t.Helper()
	noSeqScan(t, tx)
	// A simple query on the connection itself: pgx would ask for the
	// placeholders' values.
	res, err := tx.Conn().PgConn().Exec(context.Background(), `EXPLAIN (GENERIC_PLAN, FORMAT JSON) `+query).ReadAll()
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 {
		t.Fatalf("explain: %d results", len(res))
	}
	plan := res[0].Rows[0][0]
	return scans(t, relation, plan), plan
}

func noSeqScan(t testing.TB, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(context.Background(), `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
}

// scans is the index condition of each scan of relation in plan.
func scans(t testing.TB, relation string, plan []byte) []string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(plan, &doc); err != nil {
		t.Fatal(err)
	}
	var conds []string
	var walk func(any)
	walk = func(v any) {
		switch v := v.(type) {
		case map[string]any:
			if v["Relation Name"] == relation {
				c, _ := v["Index Cond"].(string)
				conds = append(conds, c)
			}
			for _, c := range v {
				walk(c)
			}
		case []any:
			for _, c := range v {
				walk(c)
			}
		}
	}
	walk(doc)
	return conds
}

// ReadsByTarget fails the test unless every scan of relation in query's
// plan has target in its index condition: it reads one conversation's
// rows, not all of an agent's.
func ReadsByTarget(t testing.TB, tx pgx.Tx, relation, query string, args ...any) {
	t.Helper()
	conds, plan := IndexConds(t, tx, relation, query, args...)
	readsByTarget(t, relation, conds, plan)
}

// GenericReadsByTarget is ReadsByTarget for the generic plan
// (GenericIndexConds): a cached plan of a prepared statement still reads
// one conversation's rows.
func GenericReadsByTarget(t testing.TB, tx pgx.Tx, relation, query string) {
	t.Helper()
	conds, plan := GenericIndexConds(t, tx, relation, query)
	readsByTarget(t, relation, conds, plan)
}

func readsByTarget(t testing.TB, relation string, conds []string, plan []byte) {
	t.Helper()
	if len(conds) == 0 {
		t.Fatalf("the plan doesn't read %s: %s", relation, plan)
	}
	for _, c := range conds {
		if !strings.Contains(c, "target = ") {
			t.Errorf("reads %s without target in its index condition: %q (all: %q)\nplan: %s", relation, c, conds, plan)
			return
		}
	}
}
