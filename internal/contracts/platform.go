package contracts

import (
	"context"
	"time"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
	"github.com/google/uuid"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

type Draft struct {
	Name               string `json:"name"`
	SQLUp              string `json:"sql_up"`
	SQLDown            string `json:"sql_down"`
	Irreversible       bool   `json:"irreversible"`
	IrreversibleReason string `json:"irreversible_reason"`
}

type Migration struct {
	Draft
	ID           string    `json:"id"`
	DatabaseID   string    `json:"database_id"`
	RevisionID   string    `json:"revision_id"`
	Version      *int      `json:"version"`
	DBMS         string    `json:"dbms"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"created_at"`
	CreatedBy    string    `json:"created_by"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CanEdit      bool      `json:"can_edit"`
}

type ChangeInput struct {
	MigrationID       string `json:"migration_id"`
	RevisionID        string `json:"revision_id"`
	Action            string `json:"action"`
	TargetExecutionID string `json:"target_execution_id,omitempty"`
	Mode              string `json:"mode"`
	BatchSize         int    `json:"batch_size"`
	CursorColumn      string `json:"cursor_column,omitempty"`
	Scope             string `json:"scope,omitempty"`
}

type Approval struct {
	GroupID    int64  `json:"group_id"`
	GroupName  string `json:"group_name"`
	LeaderID   int64  `json:"leader_id"`
	LeaderName string `json:"leader_name"`
	Status     string `json:"status"`
	Comment    string `json:"comment,omitempty"`
}

type Review struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
}

type ChangeRequest struct {
	ChangeInput
	ID            string     `json:"id"`
	DatabaseID    string     `json:"database_id"`
	MigrationName string     `json:"migration_name"`
	Status        string     `json:"status"`
	Approvals     []Approval `json:"approvals"`
	Validation    []string   `json:"validation"`
	CanApprove    bool       `json:"can_approve"`
	CanSubmit     bool       `json:"can_submit"`
	CanEnqueue    bool       `json:"can_enqueue"`
	SQL           string     `json:"sql"`
}

type Execution struct {
	ID                 string    `json:"id"`
	MigrationID        string    `json:"migration_id"`
	MigrationName      string    `json:"migration_name"`
	RevisionID         string    `json:"revision_id"`
	Action             string    `json:"action"`
	Status             string    `json:"status"`
	Actor              string    `json:"actor"`
	At                 time.Time `json:"at"`
	SQL                string    `json:"sql"`
	Attempt            int       `json:"attempt"`
	RevertsExecutionID string    `json:"reverts_execution_id,omitempty"`
	CanRollback        bool      `json:"can_rollback"`
	CanForward         bool      `json:"can_forward"`
}

type Job struct {
	ID               string    `json:"id"`
	DatabaseID       string    `json:"database_id"`
	RequestID        string    `json:"request_id"`
	MigrationID      string    `json:"migration_id"`
	ExecutionID      string    `json:"execution_id"`
	Status           string    `json:"status"`
	CommittedRows    int64     `json:"committed_rows"`
	CommittedBatches int64     `json:"committed_batches"`
	TotalRows        *int64    `json:"total_rows"`
	Attempt          int       `json:"attempt"`
	LastError        string    `json:"last_error,omitempty"`
	CanResume        bool      `json:"can_resume"`
	CanCompensate    bool      `json:"can_compensate"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Leader struct {
	GroupID  int64  `json:"group_id"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
}

type Workflow interface {
	List(context.Context, reqctx.Principal, uuid.UUID) ([]Migration, error)
	Get(context.Context, reqctx.Principal, uuid.UUID, string) (Migration, error)
	Save(context.Context, reqctx.Principal, uuid.UUID, string, Draft) (Migration, error)
	SaveGenerated(context.Context, reqctx.Principal, uuid.UUID, string, []string) ([]Migration, error)
	Requests(context.Context, reqctx.Principal, uuid.UUID) ([]ChangeRequest, error)
	Request(context.Context, reqctx.Principal, uuid.UUID, string) (ChangeRequest, error)
	Propose(context.Context, reqctx.Principal, uuid.UUID, ChangeInput) (ChangeRequest, error)
	Submit(context.Context, reqctx.Principal, uuid.UUID, string) (ChangeRequest, error)
	Review(context.Context, reqctx.Principal, uuid.UUID, string, Review) (ChangeRequest, error)
	Enqueue(context.Context, reqctx.Principal, uuid.UUID, string, string) (Job, error)
	History(context.Context, reqctx.Principal, uuid.UUID, bool) ([]Execution, error)
}

type Jobs interface {
	List(context.Context, reqctx.Principal, uuid.UUID) ([]Job, error)
	Get(context.Context, reqctx.Principal, uuid.UUID, string) (Job, error)
	Recover(context.Context, reqctx.Principal, uuid.UUID, string, string, string) (ChangeRequest, error)
}

type Leaders interface {
	List(context.Context, reqctx.Principal) ([]Leader, error)
	Set(context.Context, reqctx.Principal, int64, int64) error
}
