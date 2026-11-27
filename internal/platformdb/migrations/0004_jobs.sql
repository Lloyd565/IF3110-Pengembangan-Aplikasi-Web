CREATE TABLE jobs (
    id                bigserial PRIMARY KEY,
    connection_id     uuid        NOT NULL REFERENCES db_connections(id),
    request_id        bigint      NOT NULL,
    execution_id      bigint      NOT NULL,
    idempotency_key   text        NOT NULL UNIQUE,
    operation         text        NOT NULL CHECK (operation IN ('apply','rollback','forward','resume','compensate')),
    mode              text        NOT NULL DEFAULT 'normal' CHECK (mode IN ('normal','batch')),
    sql               text        NOT NULL,
    batch_size        int         CHECK (batch_size > 0),
    batch_table       text,
    batch_key         text,
    batch_scope_max   bigint,
    status            text        NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued','running','retry_wait','succeeded','failed','cancelled')),
    hold              text        NOT NULL DEFAULT 'none'
                      CHECK (hold IN ('none','partial','partially_reverted','commit_unknown')),
    retry_count       int         NOT NULL DEFAULT 0,
    max_retries       int         NOT NULL DEFAULT 3,
    next_retry_at     timestamptz,
    worker_id         text,
    lease_until       timestamptz,
    committed_batches bigint      NOT NULL DEFAULT 0,
    committed_rows    bigint      NOT NULL DEFAULT 0,
    error_message     text,
    trace_context     text        NOT NULL DEFAULT '',
    queued_at         timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz,
    CHECK ((mode = 'batch') = (batch_size IS NOT NULL AND batch_table IS NOT NULL AND batch_key IS NOT NULL))
);
CREATE INDEX jobs_claim_idx ON jobs (status, next_retry_at, lease_until);
CREATE INDEX jobs_connection_idx ON jobs (connection_id, status);
