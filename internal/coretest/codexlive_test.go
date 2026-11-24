package coretest_test

import (
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/codexconnector"
	"github.com/wakeshift/server/internal/coretest"
	"github.com/wakeshift/server/internal/streams"
)

// TestCodexConnectorLive (spike) runs the connector against a real Codex
// thread: WAKESHIFT_CODEX_THREAD names a loaded thread on the app-server
// daemon. Each event starts a real turn in it.
func TestCodexConnectorLive(t *testing.T) {
	thread := os.Getenv("WAKESHIFT_CODEX_THREAD")
	if thread == "" {
		t.Skip("set WAKESHIFT_CODEX_THREAD to a Codex thread to deliver into")
	}
	socket := os.ExpandEnv("$HOME/.codex/app-server-control/app-server-control.sock")
	c := coretest.Start(t)
	c.Streams.MaxAge = 10 * time.Second // rotate 10s after connecting (the connection's age limit)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")

	link, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Conversation: thread, Ack: true})
	if err != nil {
		t.Fatal(err)
	}
	lineSub(t, c, app, codex, link.Session)
	states := func() map[string]string {
		out := map[string]string{}
		c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
			rows, _ := tx.Query(ctx, `SELECT e.subject, d.state FROM deliveries d JOIN events e ON e.tenant_id = d.tenant_id AND e.id = d.event_id
				WHERE d.agent_id = 'codex'`)
			for rows.Next() {
				var s, st string
				rows.Scan(&s, &st)
				subject, _, _ := strings.Cut(s, ":")
				out[subject] = st
			}
			return rows.Err()
		})
		return out
	}
	acked := func(subjects ...string) {
		t.Helper()
		deadline := time.Now().Add(90 * time.Second)
		for {
			got := states()
			ok := true
			for _, s := range subjects {
				ok = ok && got[s] == "acked"
			}
			if ok {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("not acked: %v", got)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	post := func(subject string) {
		t.Helper()
		postLine(t, c, app, codex, subject+": a test message from a Wakeshift spike. Reply in one short sentence; don't call any tools.")
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- codexconnector.Run(ctx, codexconnector.Config{Stream: link.URL, Issuers: []string{c.URL}, Thread: thread, Socket: socket,
			Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	}()

	post("one")
	acked("one")
	t.Logf("one acked after %v", time.Since(start))

	// Two at once: the second waits for the first turn to end.
	post("two")
	post("three")
	acked("two", "three")
	t.Logf("two and three acked after %v", time.Since(start))

	// After the link has rotated, events still arrive.
	time.Sleep(time.Until(start.Add(15 * time.Second)))
	post("four")
	acked("four")
	t.Logf("four acked after %v", time.Since(start))

	// Revoking the agent's links (as unwatch would revoke the session's)
	// stops the connector.
	revoked := time.Now()
	c.DB.InTenant(ctx, codex.Workspace, func(tx pgx.Tx) error {
		_, err := streams.RevokeLinks(ctx, tx, "codex")
		return err
	})
	select {
	case err := <-done:
		if !errors.Is(err, codexconnector.ErrRevoked) {
			t.Fatalf("exited with %v", err)
		}
		t.Logf("exited %v after revocation: %v", time.Since(revoked), err)
	case <-time.After(10 * time.Second):
		t.Fatal("still running 10s after revocation")
	}
}

// TestCodexConnectorLiveEnds (spike): an unused thread's lease runs out;
// then, with no lease, the connector runs until the thread is archived
// (by hand, within a minute: thread/archive on the app-server).
func TestCodexConnectorLiveEnds(t *testing.T) {
	thread := os.Getenv("WAKESHIFT_CODEX_THREAD")
	if thread == "" {
		t.Skip("set WAKESHIFT_CODEX_THREAD to a Codex thread to deliver into")
	}
	socket := os.ExpandEnv("$HOME/.codex/app-server-control/app-server-control.sock")
	c := coretest.Start(t)
	app := chatApp(t, c)
	codex := actAs(t, app, "ada", "codex")
	run := func(lease, within time.Duration) error {
		link, err := agentkit.SessionLink(ctx, codex, agentkit.LinkRequest{Conversation: thread, Ack: true})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			done <- codexconnector.Run(ctx, codexconnector.Config{Stream: link.URL, Issuers: []string{c.URL}, Thread: thread, Socket: socket, Lease: lease})
		}()
		select {
		case err := <-done:
			t.Logf("exited after %v: %v", time.Since(start), err)
			return err
		case <-time.After(within):
			t.Fatalf("still running after %v", within)
			return nil
		}
	}
	if err := run(4*time.Second, 10*time.Second); !errors.Is(err, codexconnector.ErrLease) {
		t.Fatalf("lease: %v", err)
	}
	if os.Getenv("WAKESHIFT_CODEX_ARCHIVE") != "" {
		if err := run(0, time.Minute); !errors.Is(err, codexconnector.ErrThreadEnded) {
			t.Fatalf("archive: %v", err)
		}
	}
}
