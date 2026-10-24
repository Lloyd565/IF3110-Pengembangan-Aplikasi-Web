package authz

import (
	"errors"

	"github.com/google/uuid"
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

var required = map[Action]Level{
	SchemaView:       LevelView,
	DataView:         LevelView,
	MigrationView:    LevelView,
	MigrationCreate:  LevelEdit,
	MigrationUpdate:  LevelEdit,
	MigrationExecute: LevelEdit,
}

func rank(l Level) int {
	switch l {
	case LevelView:
		return 1
	case LevelEdit:
		return 2
	}
	return 0
}

func Allows(have Level, act Action) bool {
	need, ok := required[act]
	return ok && rank(have) >= rank(need)
}

type ConnAccess struct {
	ConnectionID uuid.UUID
	Name         string
	Level        Level
}
