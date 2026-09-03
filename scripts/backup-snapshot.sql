\set ON_ERROR_STOP on
-- Canonical full-row snapshots of every application/migration table. The
-- database is disposable and the source API is stopped during comparisons.
SELECT format(
    'SELECT jsonb_build_object(%L, COALESCE(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text), ''[]''::jsonb)) FROM %I.%I AS t;',
    tablename, schemaname, tablename
)
FROM pg_tables
WHERE schemaname = 'public'
ORDER BY tablename
\gexec
