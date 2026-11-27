-- Mengganti tabel jobs dari 0004 (satu eksekusi = satu baris dengan SQL dan parameter batch langsung)
-- dengan skema yang mengikuti kontrak workflow: request_id UUID, snapshot workflow.Plan, kind
-- run/resume/compensate, root_job_id untuk bukti di target, dan penanda attempt yang masih terbuka.
-- 0004 sengaja tidak diubah karena sudah dipublikasikan; database yang sudah menjalankannya ikut
-- ter-upgrade lewat file ini. Tabel lama tidak pernah ditulis kode produksi (hanya test), jadi aman di-drop.
DROP TABLE IF EXISTS jobs;

CREATE TABLE jobs (
    id                bigserial PRIMARY KEY,
    connection_id     uuid        NOT NULL REFERENCES db_connections(id),
    request_id        uuid        NOT NULL,
    idempotency_key   text        NOT NULL UNIQUE,
    kind              text        NOT NULL DEFAULT 'run' CHECK (kind IN ('run','resume','compensate')),
    parent_job_id     bigint      REFERENCES jobs(id),
    root_job_id       bigint      NOT NULL,
    plan              jsonb       NOT NULL,
    status            text        NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued','running','retry_wait','success','failed','cancelled')),
    hold              text        NOT NULL DEFAULT 'none'
                      CHECK (hold IN ('none','partial','partially_reverted','commit_unknown')),
    attempt           int         NOT NULL DEFAULT 0,
    retry_count       int         NOT NULL DEFAULT 0,
    max_retries       int         NOT NULL DEFAULT 3,
    next_retry_at     timestamptz,
    worker_id         text,
    lease_until       timestamptz,
    execution_id      uuid,
    attempt_open      boolean     NOT NULL DEFAULT false,
    committed_batches bigint      NOT NULL DEFAULT 0,
    committed_rows    bigint      NOT NULL DEFAULT 0,
    total_rows        bigint,
    error_message     text,
    trace_context     text        NOT NULL DEFAULT '',
    queued_at         timestamptz NOT NULL DEFAULT now(),
    started_at        timestamptz,
    finished_at       timestamptz,
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_claim_idx ON jobs (status, next_retry_at, lease_until);
CREATE INDEX jobs_connection_idx ON jobs (connection_id, status);
CREATE INDEX jobs_request_idx ON jobs (request_id);
