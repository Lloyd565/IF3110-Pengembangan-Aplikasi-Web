-- 0001_init: skema Platform DB (owner: C). Dibekukan setelah sesi kontrak.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TABLE users (
    id                bigserial PRIMARY KEY,
    username          citext      NOT NULL UNIQUE,
    password_hash     text        NOT NULL,
    is_platform_admin boolean     NOT NULL DEFAULT false,
    is_active         boolean     NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE groups (
    id          bigserial PRIMARY KEY,
    name        text        NOT NULL UNIQUE,
    description text        NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id bigint NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id  bigint NOT NULL REFERENCES users(id)  ON DELETE CASCADE,
    PRIMARY KEY (group_id, user_id)
);
CREATE INDEX group_members_user_id_idx ON group_members (user_id);

-- id dibuat di aplikasi sebelum INSERT (dipakai sebagai AD AES-GCM), jadi tanpa DEFAULT.
-- Hapus = soft-delete (deleted_at) agar riwayat migration tetap ada.
CREATE TABLE db_connections (
    id           uuid        PRIMARY KEY,
    name         text        NOT NULL,
    host         text        NOT NULL,
    port         int         NOT NULL CHECK (port BETWEEN 1 AND 65535),
    dbname       text        NOT NULL,
    username     text        NOT NULL,
    password_enc bytea       NOT NULL,
    sslmode      text        NOT NULL DEFAULT 'disable'
                 CHECK (sslmode IN ('disable','allow','prefer','require','verify-ca','verify-full')),
    created_by   bigint      REFERENCES users(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz
);
CREATE UNIQUE INDEX db_connections_name_live_idx ON db_connections (name) WHERE deleted_at IS NULL;

CREATE TABLE db_grants (
    group_id      bigint NOT NULL REFERENCES groups(id)         ON DELETE CASCADE,
    connection_id uuid   NOT NULL REFERENCES db_connections(id) ON DELETE CASCADE,
    level         text   NOT NULL CHECK (level IN ('view','edit')),
    PRIMARY KEY (group_id, connection_id)
);
CREATE INDEX db_grants_connection_id_idx ON db_grants (connection_id);

-- token_hash/prev_token_hash = sha256(token). Token mentah tidak pernah disimpan.
CREATE TABLE sessions (
    id                  bigserial PRIMARY KEY,
    user_id             bigint      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash          bytea       NOT NULL UNIQUE,
    prev_token_hash     bytea,
    prev_valid_until    timestamptz,
    csrf_token          text        NOT NULL,
    force_rotate        boolean     NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    last_seen_at        timestamptz NOT NULL DEFAULT now(),
    rotated_at          timestamptz NOT NULL DEFAULT now(),
    absolute_expires_at timestamptz NOT NULL,
    revoked_at          timestamptz
);
CREATE INDEX sessions_user_id_idx ON sessions (user_id);
CREATE INDEX sessions_prev_token_hash_idx ON sessions (prev_token_hash) WHERE prev_token_hash IS NOT NULL;

-- version NULL selama draft, diisi MAX+1 saat eksekusi.
CREATE TABLE migrations (
    id            bigserial PRIMARY KEY,
    connection_id uuid        NOT NULL REFERENCES db_connections(id),
    version       int,
    name          text        NOT NULL,
    sql           text        NOT NULL,
    status        text        NOT NULL DEFAULT 'draft'
                  CHECK (status IN ('draft','running','success','failed')),
    error_message text,
    created_by    bigint      NOT NULL REFERENCES users(id),
    executed_by   bigint      REFERENCES users(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    started_at    timestamptz,
    finished_at   timestamptz,
    UNIQUE (connection_id, version),
    CHECK ((status = 'draft') = (version IS NULL))
);

CREATE TABLE audit_log (
    id         bigserial PRIMARY KEY,
    actor_id   bigint      REFERENCES users(id),
    action     text        NOT NULL,
    target     text        NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
