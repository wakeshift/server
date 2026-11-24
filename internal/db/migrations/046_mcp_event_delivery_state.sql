-- MCP Events webhooks follow the current SEP-3415 draft (internal/mcpevents):
-- per-subscription delivery state for strict ordering (one delivery in
-- flight, the next waiting for the one being retried), the retry window,
-- cursor replay, gap reporting and the hourly re-authorization. Defaults
-- fill existing rows (all ChatGPT's), so no per-workspace data step is
-- needed.
ALTER TABLE mcp_event_webhooks
    -- What the client speaks (event.DialectDraft, event.DialectChatGPT):
    -- its error codes, and whether it gets gap and terminated envelopes.
    ADD COLUMN dialect         text        NOT NULL DEFAULT 'chatgpt',
    -- The instance sending this webhook's deliveries, and until when: one
    -- at a time, in order.
    ADD COLUMN busy_holder     text,
    ADD COLUMN busy_until      timestamptz,
    -- The delivery at the head is being retried: nothing after it goes
    -- until it's acknowledged or abandoned.
    ADD COLUMN retry_attempts  integer     NOT NULL DEFAULT 0,
    ADD COLUMN retrying_since  timestamptz,
    ADD COLUMN next_attempt_at timestamptz,
    -- Replay from a client's cursor: deliveries already acknowledged or
    -- abandoned with event_seq in (replay_after, replay_through] go again,
    -- in order, before anything new.
    ADD COLUMN replay_after    bigint,
    ADD COLUMN replay_through  bigint,
    -- Events were skipped since the client last heard: its next refresh
    -- says truncated.
    ADD COLUMN truncated       boolean     NOT NULL DEFAULT false,
    -- When access was last checked (subscribe, refresh, or the hourly
    -- recheck).
    ADD COLUMN authorized_at   timestamptz NOT NULL DEFAULT now(),
    -- Ended (revoked in the console): nothing more is delivered; the next
    -- pass sends the receiver a terminated envelope with this reason and
    -- removes the webhook.
    ADD COLUMN ending_reason   text;
