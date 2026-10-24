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

type Level string

const (
	LevelNone Level = ""
	LevelView Level = "view"
	LevelEdit Level = "edit"
)

var ErrForbidden = errors.New("authz: akses ditolak")

type ConnAccess struct {
	ConnectionID uuid.UUID
	Name         string
	Level        Level
}

type Authorizer struct {
	DB *pgxpool.Pool
}

func (a *Authorizer) Can(ctx context.Context, p reqctx.Principal, connID uuid.UUID, act Action) error {
	// Fail closed until the access/admin slice implements the grant policy.
	return ErrForbidden
}

func (a *Authorizer) VisibleConnections(ctx context.Context, p reqctx.Principal) ([]ConnAccess, error) {
	return nil, nil
}
