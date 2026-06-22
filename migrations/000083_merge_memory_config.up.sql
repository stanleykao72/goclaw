-- merge_memory_config: collapse the two public agent memory knobs
-- other_config.memory_mode {notebook,vault,both} + other_config.memory_backend
-- {db,vault} into ONE public field other_config.memory with 5 values
-- {notebook, vault, db, both-vault, both-db}. The internal (mode, backend) gating
-- is unchanged — only the SOURCE collapses. ParseMemory() in Go decomposes the
-- single field back into the (mode, backend) pair; un-migrated rows fall back to
-- the legacy keys.
--
-- This is a JSONB DATA migration (no schema change). It is idempotent-ish:
--   * only rows that HAVE a legacy key (memory_mode OR memory_backend) are touched;
--   * rows that already have 'memory' are skipped (re-run safe);
--   * rows with NEITHER legacy key are left untouched and default to both-db at
--     read time via ParseMemory().
-- other_config is `JSONB NOT NULL DEFAULT '{}'` (see 000001), so the `?` / `->>`
-- operators below never see NULL; empty-config rows ('{}') match no legacy key
-- and are correctly skipped.
--
-- Decomposition (mode wins for notebook → backend forced to db; this is why the
-- DOWN expansion of a `notebook` row yields backend=db even if the original row
-- carried memory_backend=vault — backend is inert when mode=notebook, so this is
-- acceptable and documented in the .down.sql):
--   (notebook, *)      -> 'notebook'
--   (vault,    vault)  -> 'vault'
--   (vault,    db)     -> 'db'
--   (both,     vault)  -> 'both-vault'
--   (both,     db)     -> 'both-db'   (and the ELSE catch-all == default)

UPDATE agents
SET other_config =
    (other_config - 'memory_mode' - 'memory_backend')
    || jsonb_build_object(
        'memory',
        CASE
            WHEN COALESCE(other_config ->> 'memory_mode', 'both') = 'notebook'
                THEN 'notebook'
            WHEN COALESCE(other_config ->> 'memory_mode', 'both') = 'vault'
                 AND COALESCE(other_config ->> 'memory_backend', 'db') = 'vault'
                THEN 'vault'
            WHEN COALESCE(other_config ->> 'memory_mode', 'both') = 'vault'
                 AND COALESCE(other_config ->> 'memory_backend', 'db') = 'db'
                THEN 'db'
            WHEN COALESCE(other_config ->> 'memory_mode', 'both') = 'both'
                 AND COALESCE(other_config ->> 'memory_backend', 'db') = 'vault'
                THEN 'both-vault'
            ELSE 'both-db'
        END
    )
WHERE (other_config ? 'memory_mode' OR other_config ? 'memory_backend')
  AND NOT (other_config ? 'memory');
