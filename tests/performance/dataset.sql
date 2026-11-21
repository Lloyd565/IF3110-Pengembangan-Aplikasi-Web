\set ON_ERROR_STOP on
\if :{?rows}
\else
  \set rows 2000000
\endif

DROP SCHEMA IF EXISTS perf CASCADE;
CREATE SCHEMA perf;
CREATE TABLE perf.accounts (
    id         bigint PRIMARY KEY,
    balance    bigint      NOT NULL,
    tier       text        NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO perf.accounts (id, balance, tier)
SELECT g, (g::bigint * 7919) % 100000, CASE WHEN g % 10 = 0 THEN 'gold' ELSE 'basic' END
FROM generate_series(1, :rows) AS g;
ANALYZE perf.accounts;
SELECT count(*) AS rows, pg_size_pretty(pg_total_relation_size('perf.accounts')) AS size FROM perf.accounts;
