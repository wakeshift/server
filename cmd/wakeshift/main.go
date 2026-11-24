// Command wakeshift runs the Wakeshift server. See internal/config for its
// environment.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/wakeshift/server/apps/agentkit"
	"github.com/wakeshift/server/internal/agenthooks"
	"github.com/wakeshift/server/internal/auth"
	"github.com/wakeshift/server/internal/config"
	"github.com/wakeshift/server/internal/core"
	"github.com/wakeshift/server/internal/db"
	"github.com/wakeshift/server/internal/harnesses"
	"github.com/wakeshift/server/internal/mail"
	"github.com/wakeshift/server/internal/telemetry"
	"github.com/wakeshift/server/internal/version"
	"github.com/wakeshift/server/web"
)

func main() {
	if err := run(); err != nil {
		slog.Error("wakeshift: exiting", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}
	// JSON logs use Cloud Logging's field names, so severities and messages
	// show up properly.
	handler := agentkit.NewLogHandler(os.Stderr)
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(handler))

	// SIGTERM (Cloud Run's shutdown signal) and Ctrl-C stop the server
	// gracefully.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Traces and logs go out only when OTEL_EXPORTER_OTLP_ENDPOINT is set,
	// scrubbed (internal/telemetry); logs also to stderr as ever.
	tracing, flushTraces, err := telemetry.Start(ctx, "wakeshift", version.Version)
	if err != nil {
		return err
	}
	logs, flushLogs, err := telemetry.StartLogs(ctx, "wakeshift", version.Version)
	if err != nil {
		return err
	}
	// The exporters' own errors go to stderr only: exported, a failing
	// export would feed itself.
	local := slog.New(handler)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { local.Warn("telemetry: exporting failed", "err", err) }))
	slog.SetDefault(slog.New(telemetry.LogHandler(handler, logs, cfg.OTLPLogLevel)))
	if cfg.Profiler {
		startProfiler()
	}

	database, err := db.OpenWithOwner(ctx, cfg.DatabaseURL, cfg.MigrateDatabaseURL, db.AdminReadLogin(cfg.AdminDBUser))
	if err != nil {
		return err
	}
	defer database.Close()

	var google *auth.Google
	if cfg.GoogleClientID != "" {
		google = &auth.Google{ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret}
	}
	var github *auth.GitHub
	if cfg.GitHubClientID != "" {
		github = &auth.GitHub{ClientID: cfg.GitHubClientID, ClientSecret: cfg.GitHubClientSecret}
	}
	var sender mail.Sender
	if cfg.ResendKey != "" || cfg.ResendKeySecret != "" {
		sender = &mail.Resend{From: cfg.MailFrom, Key: cfg.ResendKey, KeySecret: cfg.ResendKeySecret}
	}
	// Install commands point at the installers' release, when there's one
	// built for this Wakeshift (signed installers).
	harnesses.SetInstallerURL(cfg.InstallerURL)
	harnesses.SetAgentPlugins(cfg.AgentPlugins)
	offerPackages(ctx, cfg)
	harnesses.SetPluginRepo("antigravity", cfg.AntigravityPlugin)
	c, err := core.New(ctx, core.Options{
		DB: database, PublicURL: cfg.PublicURL, SiteURL: cfg.SiteURL, Key: cfg.Key, Apps: cfg.Apps,
		TermsURL: cfg.TermsURL, TermsVersion: cfg.TermsVersion, PrivacyURL: cfg.PrivacyURL, SupportEmail: cfg.SupportEmail,
		AllowedSignups: cfg.AllowedSignups, DevSignIn: cfg.DevSignIn, DevSignInToken: cfg.DevSignInToken, Google: google, GitHub: github, InvitesPerUser: cfg.InvitesPerUser, Web: web.Dist, ConnectorDir: cfg.ConnectorDir,
		MaxStreams: cfg.MaxStreams, Connectors: cfg.Connectors, Mail: sender,
		AdminEmails:         cfg.AdminEmails,
		AdminServiceAccount: cfg.AdminServiceAccount, AdminIAPAudience: cfg.AdminIAPAudience,
		Tracing:            tracing,
		SubscribeVouchers:  cfg.SubscribeVouchers,
		RedeemByToolCall:   cfg.RedeemByToolCall,
		InlineSessionSteps: cfg.InlineSessionSteps,
		CodexElicit:        cfg.CodexElicit,
		MCPEventsPush:      cfg.MCPEventsPush,
		GrokWebhooks:       cfg.GrokWebhooks, GrokLinkTTL: cfg.Grok.LinkTTL, GrokRateLimits: cfg.Grok.RateLimits, ProxyHops: cfg.ProxyHops,
		GrokPacing: agenthooks.CursorPacing{Spacing: cfg.Grok.Spacing, PerHour: cfg.Grok.PerHour, PerDay: cfg.Grok.PerDay,
			BatchEvents: cfg.Grok.BatchEvents, BatchBytes: cfg.Grok.BatchBytes},
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           c.Handler,
		ReadHeaderTimeout: 10 * time.Second,
		// No read or write timeout (streams and long polls stay open), but
		// an idle keep-alive connection closes.
		IdleTimeout: 2 * time.Minute,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	runnerDone := make(chan struct{})
	go func() {
		defer close(runnerDone)
		c.Run(ctx)
	}()
	slog.Info("wakeshift: listening", "addr", cfg.Addr, "version", version.Version, "public_url", cfg.PublicURL, "subscribe_vouchers", cfg.SubscribeVouchers, "mcp_events_push", cfg.MCPEventsPush, "grok_webhooks", cfg.GrokWebhooks)
	if cfg.DevSignInTokenMade {
		// Straight to stderr, never through slog: log lines can be exported
		// (internal/telemetry), and this is a credential for this run only.
		fmt.Fprintf(os.Stderr, "\nDevelopment sign-in: open %s/?dev_token=%s\n(or enter the token %s on the sign-in page)\n\n", cfg.PublicURL, cfg.DevSignInToken, cfg.DevSignInToken)
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Cloud Run kills the process 10s after SIGTERM: finish well inside that.
	slog.Info("wakeshift: shutting down")
	// Streams are hijacked connections that Shutdown doesn't close: ask
	// their clients to reconnect (to another instance) first.
	drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
	c.Streams.Drain(drainCtx)
	cancelDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Warn("wakeshift: requests still running at shutdown", "err", err)
	}
	// The last traces and logs go out while the runner stops, in the same
	// time (lines logged after the flush reach stderr only).
	flushCtx, cancelFlush := context.WithTimeout(context.Background(), runnerWait)
	defer cancelFlush()
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		if err := flushTraces(flushCtx); err != nil {
			local.Warn("wakeshift: traces not flushed at shutdown", "err", err)
		}
		if err := flushLogs(flushCtx); err != nil {
			local.Warn("wakeshift: logs not flushed at shutdown", "err", err)
		}
	}()
	// The runner's current step commits or rolls back; don't wait past
	// Cloud Run's SIGKILL for one stuck on the database (an uncommitted
	// step rolls back when the process exits).
	select {
	case <-runnerDone:
	case <-time.After(runnerWait):
		slog.Warn("wakeshift: the pipeline runner hadn't stopped at shutdown")
	}
	<-flushed
	return nil
}

// runnerWait bounds the wait for the pipeline runner at shutdown: with the
// 5s drain and 3s Shutdown, it ends inside Cloud Run's 10s.
const runnerWait = 1500 * time.Millisecond
