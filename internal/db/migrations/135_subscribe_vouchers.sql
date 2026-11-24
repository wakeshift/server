-- Subscribe vouchers (docs/subscribe-vouchers.md): when one of an app's
-- tools subscribes a native MCP Events client (ChatGPT) to a tools-only
-- event, /calls/addition hands it a voucher (wsv_…) in the tool's result,
-- and the client's events/subscribe redeems it for exactly one webhook
-- subscription to that event with those arguments, within an hour.
--
-- Platform-level, with no tenant_id policy, and exempt in
-- TestEveryTenantTableIsIsolated: an unused voucher has no workspace (the
-- redemption picks it), and a row holds no tenant data. It holds the
-- token's SHA-256 (never the token), the app, the event's name, the
-- arguments' key (apps.ArgumentsKey: already a SHA-256; never the
-- arguments), the app's ID for the user it was handed to (which tenants
-- already keeps at platform level), a hash of the hand-out's MCP transport
-- session, the tool's name and call.subscribe's options. The subscription
-- it makes, with its callback URL and sealed secret, lives in a workspace
-- under row-level security like every native subscription. Rows go with
-- their app (ON DELETE CASCADE; a deleted app's are deleted by
-- appapi.finishAppDeletion) and, once used, with the workspace their
-- subscription is in (tenant_id, ON DELETE CASCADE); the expiry pass
-- deletes an unused one an hour after it expires and a used one an hour
-- after its subscription stops being live (live_until).
CREATE TABLE subscribe_vouchers (
    token_hash            bytea       PRIMARY KEY,
    app_id                text        NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    event                 text        NOT NULL,
    arguments_key         text        NOT NULL,
    app_user              text,                 -- the verified user at hand-out; NULL: anonymous
    transport_session     text,                 -- SHA-256 (hex) of the hand-out's Mcp-Session-Id, anonymous only
    tool                  text        NOT NULL,
    once                  boolean     NOT NULL DEFAULT false,
    ttl                   interval,             -- call.subscribe's ttl: caps the subscription across refreshes
    replay                interval,             -- call.subscribe's replay
    issued_at             timestamptz NOT NULL DEFAULT now(),
    expires_at            timestamptz NOT NULL,
    claim_id              text,                 -- a redemption in progress
    claimed_until         timestamptz,
    verify_failures       integer     NOT NULL DEFAULT 0,
    used_at               timestamptz,
    tenant_id             uuid        REFERENCES tenants(id) ON DELETE CASCADE,
    event_subscription_id uuid,
    live_until            timestamptz,          -- used: until when its subscription may be live (its refresh_before, or when it ended)
    CHECK ((used_at IS NULL) = (tenant_id IS NULL)),
    CHECK ((used_at IS NULL) = (event_subscription_id IS NULL)),
    CHECK (app_user IS NULL OR transport_session IS NULL)
);
-- Counting unused vouchers at hand-out (per user, and anonymous per app).
CREATE INDEX subscribe_vouchers_unused ON subscribe_vouchers (app_id, app_user, expires_at) WHERE used_at IS NULL;
-- Anonymous fan-out caps at redemption, and the admin action that ends
-- live voucher subscriptions.
CREATE INDEX subscribe_vouchers_used ON subscribe_vouchers (app_id, event, arguments_key) WHERE used_at IS NOT NULL;
CREATE INDEX subscribe_vouchers_tenant ON subscribe_vouchers (tenant_id) WHERE tenant_id IS NOT NULL;
-- The expiry pass.
CREATE INDEX subscribe_vouchers_expires ON subscribe_vouchers (expires_at) WHERE used_at IS NULL;
CREATE INDEX subscribe_vouchers_live_until ON subscribe_vouchers (live_until) WHERE used_at IS NOT NULL;

-- Per-app counters for vouchers: anonymous callback verifications this
-- minute (at most 30); and, for what anyone can send without holding a
-- voucher (the anonymous cap at hand-out; a redemption whose voucher is
-- malformed or unknown), the refusals since the app's log last said so,
-- which it says at most once a minute, with the count, so probing can't
-- flood the log. An app ID, times and counts: platform-level, like
-- app_emit_counters.
CREATE TABLE subscribe_voucher_counters (
    app_id          text        PRIMARY KEY REFERENCES apps(id) ON DELETE CASCADE,
    verify_window   timestamptz NOT NULL DEFAULT 'epoch',
    verifications   integer     NOT NULL DEFAULT 0,
    cap_logged_at   timestamptz NOT NULL DEFAULT 'epoch',
    cap_refused     integer     NOT NULL DEFAULT 0,
    probe_logged_at timestamptz NOT NULL DEFAULT 'epoch',
    probe_refused   integer     NOT NULL DEFAULT 0
);
