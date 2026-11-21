-- Milestone 2: definisi/revisi, pengajuan, approval, history attempt, dan leader group.
-- Tabel migrations M1 tetap menjadi identitas definisi (id, connection, nama, version legacy).

CREATE TABLE group_leaders (
    group_id   bigint PRIMARY KEY,
    user_id    bigint      NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    -- Leader wajib anggota group; dikeluarkan dari group = otomatis bukan leader.
    FOREIGN KEY (group_id, user_id) REFERENCES group_members (group_id, user_id) ON DELETE CASCADE
);

CREATE TABLE migration_revisions (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    migration_id        bigint      NOT NULL REFERENCES migrations(id),
    number              int         NOT NULL CHECK (number >= 1),
    sql_up              text        NOT NULL,
    sql_down            text        NOT NULL DEFAULT '',
    irreversible        boolean     NOT NULL DEFAULT false,
    irreversible_reason text        NOT NULL DEFAULT '',
    digest              text        NOT NULL,
    legacy              boolean     NOT NULL DEFAULT false,
    created_by          bigint      NOT NULL REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (migration_id, number),
    CHECK (NOT irreversible OR (sql_down = '' AND irreversible_reason <> ''))
);

CREATE TABLE migration_executions (
    id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    seq                  bigserial   NOT NULL UNIQUE,
    connection_id        uuid        NOT NULL REFERENCES db_connections(id),
    migration_id         bigint      NOT NULL REFERENCES migrations(id),
    revision_id          uuid        NOT NULL REFERENCES migration_revisions(id),
    request_id           uuid,
    job_id               text,
    action               text        NOT NULL CHECK (action IN ('apply','rollback','forward')),
    attempt              int         NOT NULL DEFAULT 1 CHECK (attempt >= 1),
    status               text        NOT NULL
                         CHECK (status IN ('running','success','failed','commit_unknown','partial','partially_reverted')),
    sql                  text        NOT NULL,
    reverts_execution_id uuid        REFERENCES migration_executions(id),
    actor_id             bigint      NOT NULL REFERENCES users(id),
    error_message        text,
    legacy               boolean     NOT NULL DEFAULT false,
    started_at           timestamptz NOT NULL DEFAULT now(),
    finished_at          timestamptz,
    CHECK ((action = 'rollback') = (reverts_execution_id IS NOT NULL))
);
CREATE INDEX migration_executions_conn_idx ON migration_executions (connection_id, seq);

CREATE TABLE migration_requests (
    id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connection_id       uuid        NOT NULL REFERENCES db_connections(id),
    migration_id        bigint      NOT NULL REFERENCES migrations(id),
    revision_id         uuid        NOT NULL REFERENCES migration_revisions(id),
    action              text        NOT NULL CHECK (action IN ('apply','rollback','forward')),
    target_execution_id uuid        REFERENCES migration_executions(id),
    mode                text        NOT NULL CHECK (mode IN ('normal','batch')),
    batch_size          int         NOT NULL DEFAULT 0,
    cursor_column       text        NOT NULL DEFAULT '',
    scope               text        NOT NULL DEFAULT '',
    status              text        NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','pending_review','queued')),
    created_by          bigint      NOT NULL REFERENCES users(id),
    queued_by           bigint      REFERENCES users(id),
    created_at          timestamptz NOT NULL DEFAULT now(),
    submitted_at        timestamptz,
    queued_at           timestamptz,
    CHECK ((action = 'apply') = (target_execution_id IS NULL)),
    CHECK ((mode = 'batch') = (batch_size > 0 AND cursor_column <> ''))
);
CREATE INDEX migration_requests_conn_idx ON migration_requests (connection_id, created_at DESC);

ALTER TABLE migration_executions
    ADD FOREIGN KEY (request_id) REFERENCES migration_requests(id);

CREATE TABLE migration_approvals (
    request_id uuid        NOT NULL REFERENCES migration_requests(id),
    group_id   bigint      NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    leader_id  bigint      NOT NULL REFERENCES users(id),
    decision   text        NOT NULL CHECK (decision IN ('approve','reject')),
    comment    text        NOT NULL DEFAULT '',
    decided_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (request_id, group_id, leader_id)
);

-- Revisi immutable: perubahan SQL selalu menjadi revisi baru.
CREATE FUNCTION migration_revisions_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'revisi migration bersifat immutable' USING ERRCODE = 'restrict_violation';
END $$;
CREATE TRIGGER migration_revisions_immutable BEFORE UPDATE OR DELETE ON migration_revisions
    FOR EACH ROW EXECUTE FUNCTION migration_revisions_immutable();

-- History append-only: identitas/snapshot SQL tidak pernah berubah; status final (success/failed) dibekukan.
CREATE FUNCTION migration_executions_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' OR OLD.status IN ('success','failed')
       OR (NEW.id, NEW.seq, NEW.connection_id, NEW.migration_id, NEW.revision_id, NEW.request_id, NEW.action,
           NEW.attempt, NEW.sql, NEW.reverts_execution_id, NEW.actor_id, NEW.legacy, NEW.started_at)
          IS DISTINCT FROM
          (OLD.id, OLD.seq, OLD.connection_id, OLD.migration_id, OLD.revision_id, OLD.request_id, OLD.action,
           OLD.attempt, OLD.sql, OLD.reverts_execution_id, OLD.actor_id, OLD.legacy, OLD.started_at)
    THEN
        RAISE EXCEPTION 'migration history bersifat append-only' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END $$;
CREATE TRIGGER migration_executions_guard BEFORE UPDATE OR DELETE ON migration_executions
    FOR EACH ROW EXECUTE FUNCTION migration_executions_guard();

-- Backfill M1: setiap migration lama menjadi revisi 1 irreversible berlabel legacy.
-- Rumus digest sama dengan workflow.digest (pemisah chr(31)).
INSERT INTO migration_revisions (migration_id, number, sql_up, irreversible, irreversible_reason, digest, legacy, created_by, created_at)
SELECT id, 1, sql, true, 'Legacy M1: SQL down tidak tersedia.',
       encode(sha256(convert_to(sql || chr(31) || '' || chr(31) || 'true' || chr(31) || 'Legacy M1: SQL down tidak tersedia.', 'UTF8')), 'hex'),
       true, created_by, created_at
FROM migrations;

-- Eksekusi M1 yang sudah final menjadi history apply, berurutan sesuai version lama.
-- ponytail: migration M1 berstatus running saat upgrade tidak di-backfill; hentikan server dengan bersih sebelum upgrade.
INSERT INTO migration_executions (connection_id, migration_id, revision_id, action, status, sql, actor_id,
                                  error_message, legacy, started_at, finished_at)
SELECT m.connection_id, m.id, r.id, 'apply', m.status, m.sql, coalesce(m.executed_by, m.created_by),
       m.error_message, true, coalesce(m.started_at, m.updated_at), coalesce(m.finished_at, m.updated_at)
FROM migrations m
JOIN migration_revisions r ON r.migration_id = m.id
WHERE m.status IN ('success','failed')
ORDER BY m.connection_id, m.version;
