-- merge_memory_config (DOWN): expand the single public other_config.memory field
-- back into the legacy memory_mode + memory_backend pair, then drop 'memory'.
-- Only rows that HAVE 'memory' are touched.
--
-- Inverse table:
--   'notebook'   -> mode=notebook, backend=db
--   'vault'      -> mode=vault,    backend=vault
--   'db'         -> mode=vault,    backend=db
--   'both-vault' -> mode=both,     backend=vault
--   'both-db'    -> mode=both,     backend=db   (and the ELSE catch-all)
--
-- NOT a perfect inverse for notebook rows: the UP folds (notebook, <any backend>)
-- into 'notebook' (mode wins), so a row that originally carried
-- memory_backend=vault alongside memory_mode=notebook (e.g. the live esmith-general
-- agent) comes back as backend=db. This is acceptable because backend is inert
-- when mode=notebook — the notebook subsystem ignores the backend entirely.

UPDATE agents
SET other_config =
    (other_config - 'memory')
    || jsonb_build_object(
        'memory_mode',
        CASE other_config ->> 'memory'
            WHEN 'notebook'   THEN 'notebook'
            WHEN 'vault'      THEN 'vault'
            WHEN 'db'         THEN 'vault'
            WHEN 'both-vault' THEN 'both'
            ELSE 'both'
        END,
        'memory_backend',
        CASE other_config ->> 'memory'
            WHEN 'notebook'   THEN 'db'
            WHEN 'vault'      THEN 'vault'
            WHEN 'db'         THEN 'db'
            WHEN 'both-vault' THEN 'vault'
            ELSE 'db'
        END
    )
WHERE other_config ? 'memory';
