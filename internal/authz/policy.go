// Package authz: kontrak otorisasi (owner: B).
package authz

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type Action string

const (
	SchemaView       Action = "schema.view"
	DataView         Action = "data.view"
	MigrationView    Action = "migration.view"
	MigrationCreate  Action = "migration.create"
	MigrationUpdate  Action = "migration.update"
	MigrationExecute Action = "migration.execute"
)

// Level sama dengan nilai db_grants.level. LevelNone = tanpa grant.
type Level string

const (
	LevelNone Level = ""
	LevelView Level = "view"
	LevelEdit Level = "edit"
)

// ErrForbidden dikembalikan Can() saat ditolak. Handler memetakannya ke 403.
var ErrForbidden = errors.New("authz: akses ditolak")

type ConnAccess struct {
	ConnectionID uuid.UUID
	Name         string
	Level        Level // akses efektif = MAX dari semua group user
}

type Authorizer struct {
	DB *pgxpool.Pool
}

// Can: nil = boleh. Deny by default; error baca grant = ditolak (fail closed).
// TODO(B): STUB sementara, selalu mengizinkan. Ganti sebelum integrasi Sabtu 19.00.
func (a *Authorizer) Can(ctx context.Context, p reqctx.Principal, connID uuid.UUID, act Action) error {
	return nil
}

// VisibleConnections: koneksi (belum di-soft-delete) yang punya grant untuk user, beserta level efektifnya.
// TODO(B): STUB sementara.
func (a *Authorizer) VisibleConnections(ctx context.Context, p reqctx.Principal) ([]ConnAccess, error) {
	return nil, nil
}
