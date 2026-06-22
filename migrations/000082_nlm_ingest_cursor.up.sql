-- nlm_ingest_cursor: per-consumer high-water for the NotebookLM ingest worker
-- (sub-phase 2.3). One row per (tenant, channel, history_key) = one row per
-- scope/Doc the worker drains into.
--
-- The ingest worker is a READ-ONLY consumer of channel_pending_messages: it
-- never deletes rows (so it coexists with group-memory curation and a possibly
-- re-enabled channelmemory worker). Instead it advances this cursor past the
-- messages it has appended to the scope's Doc. The window read is
--   WHERE (created_at, id) > (last_created_at, last_id)
-- ordered by (created_at ASC, id ASC). The composite (created_at, id) cursor is
-- collision-safe: AppendBatch stamps one created_at for a whole batch, so a
-- created_at-only cursor could skip or re-emit rows on the boundary; the id
-- (UUIDv7, time-ordered) tiebreak makes the cursor total.
--
-- The cursor advances ONLY after a verified-successful AppendText + source sync
-- for the scope (fail-soft: on error the cursor is left so the window retries
-- next tick). PRIMARY KEY(tenant_id, channel_name, history_key) makes the
-- per-scope cursor the single serialization point — exactly one appender per Doc.

CREATE TABLE IF NOT EXISTS nlm_ingest_cursor (
    tenant_id       UUID        NOT NULL REFERENCES tenants(id),
    channel_name    TEXT        NOT NULL,
    history_key     TEXT        NOT NULL,
    last_created_at TIMESTAMPTZ NOT NULL DEFAULT 'epoch',
    last_id         UUID        NOT NULL DEFAULT '00000000-0000-0000-0000-000000000000',
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (tenant_id, channel_name, history_key)
);
