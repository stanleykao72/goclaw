-- nlm_notebooks: pointer table for the goclaw × NotebookLM 4-tier memory system.
-- One row per (tenant, scope) maps a memory scope to its NotebookLM notebook
-- and the backing Drive Doc. The UNIQUE(tenant_id, scope_kind, scope_id) is the
-- natural key AND the race-safe lazy-create ON CONFLICT target — there is no id
-- PK because the scope tuple already uniquely identifies a row.
--
--   scope_kind ∈ {shared, user, agent, group}
--   scope_id   = '' for shared; userId / agentKey / chatId otherwise.

CREATE TABLE IF NOT EXISTS nlm_notebooks (
    tenant_id     UUID NOT NULL REFERENCES tenants(id),
    scope_kind    TEXT NOT NULL,
    scope_id      TEXT NOT NULL DEFAULT '',
    notebook_id   TEXT NOT NULL,
    drive_doc_id  TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, scope_kind, scope_id)
);

CREATE INDEX IF NOT EXISTS idx_nlm_notebooks_tenant
    ON nlm_notebooks (tenant_id, scope_kind);
