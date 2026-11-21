#!/usr/bin/env sh
set -eu
ROWS="${ROWS:-2000000}"
cd "$(dirname "$0")/../.."
psql() { docker compose exec -T target-db psql -U target -d target -X "$@"; }

psql -q -v rows="$ROWS" -f - < tests/performance/dataset.sql
echo "UPDATE seluruh perf.accounts ($ROWS row), satu statement tanpa batch:"
psql <<'SQL'
\timing on
UPDATE perf.accounts SET balance = balance + 1, updated_at = now();
SQL
