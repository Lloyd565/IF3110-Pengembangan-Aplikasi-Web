package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/authz"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/contracts"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/migration"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/reqctx"
)

type Plan struct {
	RequestID         string
	ConnectionID      uuid.UUID
	MigrationID       string
	RevisionID        string
	Action            string
	TargetExecutionID string
	Mode              string
	BatchSize         int
	CursorColumn      string
	Scope             string
	SQL               string
	ActorID           int64
	TargetFingerprint string
	TargetIdentity    string
	TargetServer      string
	EvidenceNonce     string
}

type Queue interface {
	Enqueue(ctx context.Context, tx pgx.Tx, plan Plan, idempotencyKey string) (contracts.Job, error)
}

type Service struct {
	DB      *pgxpool.Pool
	Authz   *authz.Authorizer
	Queue   Queue
	Targets interface {
		Target(context.Context, uuid.UUID) (*dbconn.Target, error)
	}
}

var _ contracts.Workflow = (*Service)(nil)

func conflict(msg string) error { return &contracts.Error{Status: 409, Code: "conflict", Message: msg} }
func forbidden(msg string) error {
	return &contracts.Error{Status: 403, Code: "forbidden", Message: msg}
}

func digest(d contracts.Draft) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{d.SQLUp, d.SQLDown, strconv.FormatBool(d.Irreversible), d.IrreversibleReason}, "\x1f")))
	return hex.EncodeToString(sum[:])
}

func parseMigrationID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id < 1 {
		return 0, migration.ErrNotFound
	}
	return id, nil
}

func parseUUID(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, migration.ErrNotFound
	}
	return id, nil
}

func (s *Service) canEdit(ctx context.Context, p reqctx.Principal, connID uuid.UUID) bool {
	return s.Authz != nil && s.Authz.Can(ctx, p, connID, authz.MigrationExecute) == nil
}

func (s *Service) validateDraft(ctx context.Context, connID uuid.UUID, d *contracts.Draft) error {
	d.Name = strings.TrimSpace(d.Name)
	switch {
	case d.Name == "" || utf8.RuneCountInString(d.Name) > 200:
		return migration.ValidationError("Nama migration wajib diisi, maksimal 200 karakter.")
	case strings.TrimSpace(d.SQLUp) == "" || len(d.SQLUp) > 100_000 || len(d.SQLDown) > 100_000:
		return migration.ValidationError("SQL up wajib diisi; setiap SQL maksimal 100 KB.")
	case d.Irreversible && (strings.TrimSpace(d.SQLDown) != "" || strings.TrimSpace(d.IrreversibleReason) == ""):
		return migration.ValidationError("Migration irreversible memerlukan alasan dan SQL down kosong.")
	case utf8.RuneCountInString(d.IrreversibleReason) > 4000:
		return migration.ValidationError("Alasan irreversible maksimal 4000 karakter.")
	}
	if !d.Irreversible {
		d.IrreversibleReason = ""
	}
	if err := migration.CheckSQL(ctx, s.DB, connID, d.SQLUp); err != nil {
		return err
	}
	if strings.TrimSpace(d.SQLDown) != "" {
		if err := migration.CheckSQL(ctx, s.DB, connID, d.SQLDown); err != nil {
			var invalid migration.ValidationError
			if errors.As(err, &invalid) {
				return migration.ValidationError("SQL down: " + invalid.Error())
			}
			return err
		}
	}
	return nil
}

const selectMigration = `
	SELECT m.id, m.connection_id, m.name, coalesce(m.version::bigint, v.version), m.created_at, u.username::text, dc.dbms,
	       r.id, r.sql_up, r.sql_down, r.irreversible, r.irreversible_reason,
	       e.action, e.status, e.error_message, e.id IS NOT NULL
	FROM migrations m
	JOIN users u ON u.id = m.created_by
	JOIN db_connections dc ON dc.id = m.connection_id
	LEFT JOIN (
		SELECT migration_id, row_number() OVER (PARTITION BY connection_id ORDER BY min(seq)) AS version
		FROM migration_executions GROUP BY connection_id, migration_id
	) v ON v.migration_id = m.id
	JOIN LATERAL (SELECT * FROM migration_revisions WHERE migration_id = m.id ORDER BY number DESC LIMIT 1) r ON true
	LEFT JOIN LATERAL (SELECT * FROM migration_executions WHERE migration_id = m.id ORDER BY seq DESC LIMIT 1) e ON true`

func scanMigration(row pgx.Row) (contracts.Migration, error) {
	var m contracts.Migration
	var id int64
	var conn, rev uuid.UUID
	var action, status, errMsg *string
	var executed bool
	err := row.Scan(&id, &conn, &m.Name, &m.Version, &m.CreatedAt, &m.CreatedBy, &m.DBMS,
		&rev, &m.SQLUp, &m.SQLDown, &m.Irreversible, &m.IrreversibleReason,
		&action, &status, &errMsg, &executed)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, migration.ErrNotFound
	}
	if err != nil {
		return m, err
	}
	m.ID, m.DatabaseID, m.RevisionID = strconv.FormatInt(id, 10), conn.String(), rev.String()
	m.Status, m.CanEdit = "draft", !executed
	if executed {
		m.Status = *status
		if *action == "rollback" && *status == "success" {
			m.Status = "rolled_back"
		}
		if errMsg != nil {
			m.ErrorMessage = *errMsg
		}
	}
	return m, nil
}

func (s *Service) List(ctx context.Context, _ reqctx.Principal, connID uuid.UUID) ([]contracts.Migration, error) {
	rows, err := s.DB.Query(ctx, selectMigration+` WHERE m.connection_id = $1 ORDER BY coalesce(m.version::bigint, v.version) DESC NULLS FIRST, m.id DESC`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []contracts.Migration
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Service) Get(ctx context.Context, _ reqctx.Principal, connID uuid.UUID, mid string) (contracts.Migration, error) {
	id, err := parseMigrationID(mid)
	if err != nil {
		return contracts.Migration{}, err
	}
	return scanMigration(s.DB.QueryRow(ctx, selectMigration+` WHERE m.connection_id = $1 AND m.id = $2`, connID, id))
}

func (s *Service) Save(ctx context.Context, p reqctx.Principal, connID uuid.UUID, mid string, d contracts.Draft) (contracts.Migration, error) {
	if !s.canEdit(ctx, p, connID) {
		return contracts.Migration{}, forbidden("Izin Edit diperlukan.")
	}
	if err := s.validateDraft(ctx, connID, &d); err != nil {
		return contracts.Migration{}, err
	}
	var id int64
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		id, err = saveRevision(ctx, tx, p, connID, mid, d)
		return err
	})
	if err != nil {
		return contracts.Migration{}, err
	}
	return s.Get(ctx, p, connID, strconv.FormatInt(id, 10))
}

func saveRevision(ctx context.Context, tx pgx.Tx, p reqctx.Principal, connID uuid.UUID, mid string, d contracts.Draft) (int64, error) {
	var id int64
	number := 1
	if mid == "" {
		if err := tx.QueryRow(ctx, `INSERT INTO migrations (connection_id, name, sql, created_by) VALUES ($1,$2,$3,$4) RETURNING id`,
			connID, d.Name, d.SQLUp, p.UserID).Scan(&id); err != nil {
			return 0, err
		}
	} else {
		var err error
		if id, err = parseMigrationID(mid); err != nil {
			return 0, err
		}
		var executed bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM migration_executions WHERE migration_id = m.id),
			       (SELECT max(number) + 1 FROM migration_revisions WHERE migration_id = m.id)
			FROM migrations m WHERE m.id = $1 AND m.connection_id = $2 FOR UPDATE`, id, connID).Scan(&executed, &number)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, migration.ErrNotFound
		}
		if err != nil {
			return 0, err
		}
		if executed {
			return 0, conflict("Migration yang pernah dieksekusi bersifat immutable. Buat migration baru untuk perbaikan.")
		}
		if _, err := tx.Exec(ctx, `UPDATE migrations SET name = $2, sql = $3, updated_at = now() WHERE id = $1`, id, d.Name, d.SQLUp); err != nil {
			return 0, err
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO migration_revisions (migration_id, number, sql_up, sql_down, irreversible, irreversible_reason, digest, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, number, d.SQLUp, d.SQLDown, d.Irreversible, d.IrreversibleReason, digest(d), p.UserID)
	return id, err
}

func (s *Service) SaveGenerated(ctx context.Context, p reqctx.Principal, connID uuid.UUID, name string, statements []string) ([]contracts.Migration, error) {
	var ids []int64
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		for i, sql := range statements {
			d := contracts.Draft{Name: name, SQLUp: sql}
			if len(statements) > 1 {
				d.Name = fmt.Sprintf("%s (%d/%d)", name, i+1, len(statements))
			}
			if err := s.validateDraft(ctx, connID, &d); err != nil {
				return err
			}
			id, err := saveRevision(ctx, tx, p, connID, "", d)
			if err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]contracts.Migration, 0, len(ids))
	for _, id := range ids {
		m, err := s.Get(ctx, p, connID, strconv.FormatInt(id, 10))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

type querier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func loadPosition(ctx context.Context, q querier, connID uuid.UUID) (position, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, migration_id, action, status, coalesce(reverts_execution_id::text, '')
		FROM migration_executions WHERE connection_id = $1 ORDER BY seq`, connID)
	if err != nil {
		return position{}, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[attempt])
	if err != nil {
		return position{}, err
	}
	pos := replay(list)
	var held string
	err = q.QueryRow(ctx, `SELECT coalesce(j.execution_id::text,j.id::text)
 FROM jobs j WHERE j.connection_id=$1 AND j.status='failed' AND j.hold='commit_unknown'
 AND NOT EXISTS (SELECT 1 FROM jobs newer WHERE newer.root_job_id=j.root_job_id AND newer.id>j.id AND newer.status='success')
 AND NOT EXISTS (SELECT 1 FROM migration_executions e WHERE e.id=j.execution_id AND e.status='commit_unknown')
 ORDER BY j.id LIMIT 1`, connID).Scan(&held)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return position{}, err
	}
	if held != "" {
		pos.blocked = "Bukti execution " + held + " belum terverifikasi; rekonsiliasi diperlukan."
		pos.blockedID, pos.blockedStatus = held, "commit_unknown"
	}
	return pos, nil
}

func (s *Service) History(ctx context.Context, _ reqctx.Principal, connID uuid.UUID, hide bool) ([]contracts.Execution, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT e.id::text, e.migration_id, m.name, e.revision_id::text, e.action, e.status, u.username::text,
		       e.started_at, e.sql, e.executed_sql, e.sql_dispatch_tracked, e.attempt,
		       coalesce(e.reverts_execution_id::text, ''), coalesce(e.error_message, ''),
		       r.irreversible
		FROM migration_executions e
		JOIN migrations m ON m.id = e.migration_id
		JOIN migration_revisions r ON r.id = e.revision_id
		JOIN users u ON u.id = e.actor_id
		WHERE e.connection_id = $1 ORDER BY e.seq`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		contracts.Execution
		migrationID  int64
		irreversible bool
	}
	var list []row
	var history []attempt
	for rows.Next() {
		var x row
		var errMsg, plannedSQL string
		var executedSQL *string
		var dispatchTracked bool
		if err := rows.Scan(&x.ID, &x.migrationID, &x.MigrationName, &x.RevisionID, &x.Action, &x.Status, &x.Actor,
			&x.At, &plannedSQL, &executedSQL, &dispatchTracked, &x.Attempt, &x.RevertsExecutionID, &errMsg, &x.irreversible); err != nil {
			return nil, err
		}
		switch {
		case executedSQL != nil:
			x.SQL, x.SQLSource = *executedSQL, "dispatched"
		case dispatchTracked:
			x.PlannedSQL, x.SQLSource = plannedSQL, "not_dispatched"
		default:
			x.SQL, x.SQLSource = plannedSQL, "stored"
		}
		x.MigrationID = strconv.FormatInt(x.migrationID, 10)
		x.ErrorMessage = errMsg
		list = append(list, x)
		history = append(history, attempt{x.ID, x.migrationID, x.Action, x.Status, x.RevertsExecutionID})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	pos := replay(history)
	out := make([]contracts.Execution, 0, len(list))
	for _, x := range list {
		x.CanRollback = !x.irreversible && pos.check("rollback", x.migrationID, x.ID) == ""
		x.CanForward = pos.check("forward", x.migrationID, x.ID) == ""
		out = append(out, x.Execution)
	}
	if hide {
		out = hideRollback(out, func(e contracts.Execution) attempt {
			return attempt{ID: e.ID, Action: e.Action, Status: e.Status, Reverts: e.RevertsExecutionID}
		})
	}
	return out, nil
}

type request struct {
	contracts.ChangeRequest
	connID             uuid.UUID
	migrationID        int64
	stored             string
	createdBy          int64
	queuedBy           *int64
	upSQL              string
	downSQL            string
	irrev              bool
	latestRev          string
	targetMatches      bool
	invalidForwardMode bool
	fingerprint        string
	identity           string
	physical           string
}

const selectRequest = `
	SELECT q.id::text, q.connection_id, q.migration_id, m.name, q.revision_id::text, q.action,
	       coalesce(q.target_execution_id::text, ''), q.mode, q.batch_size, q.cursor_column, q.scope, q.status,
	       q.created_by, q.queued_by, r.sql_up, r.sql_down, r.irreversible,
	       (SELECT id::text FROM migration_revisions WHERE migration_id = q.migration_id ORDER BY number DESC LIMIT 1),
	       q.target_fingerprint = ` + targetFingerprint + ` AND q.target_identity = ` + dbconn.IdentityFingerprint + `,
	       q.target_fingerprint, q.target_identity, q.target_server,
	       CASE WHEN q.action = 'forward' AND q.mode = 'normal' THEN EXISTS (
	           SELECT 1 FROM migration_executions e JOIN jobs original ON original.id::text = e.job_id
	           WHERE e.id = q.target_execution_id AND original.plan->>'Mode' = 'batch'
	       ) ELSE false END,
	       coalesce(j.id::text, ''), coalesce(j.status, ''), coalesce(j.hold, '')
	FROM migration_requests q
	JOIN migrations m ON m.id = q.migration_id
	JOIN migration_revisions r ON r.id = q.revision_id
	JOIN db_connections dc ON dc.id = q.connection_id
	LEFT JOIN LATERAL (
		SELECT id, status, hold FROM jobs
		WHERE q.status = 'queued' AND request_id = q.id AND connection_id = q.connection_id
		ORDER BY id DESC LIMIT 1
	) j ON true`

func scanRequest(row pgx.Row) (request, error) {
	var x request
	var jobID, jobStatus, hold string
	err := row.Scan(&x.ID, &x.connID, &x.migrationID, &x.MigrationName, &x.RevisionID, &x.Action,
		&x.TargetExecutionID, &x.Mode, &x.BatchSize, &x.CursorColumn, &x.Scope, &x.stored,
		&x.createdBy, &x.queuedBy, &x.upSQL, &x.downSQL, &x.irrev, &x.latestRev, &x.targetMatches, &x.fingerprint, &x.identity, &x.physical, &x.invalidForwardMode,
		&jobID, &jobStatus, &hold)
	if errors.Is(err, pgx.ErrNoRows) {
		return x, migration.ErrNotFound
	}
	if jobID != "" {
		x.Job = &contracts.JobSummary{ID: jobID, Status: contracts.PublicJobStatus(jobStatus, hold)}
	}
	x.DatabaseID, x.MigrationID = x.connID.String(), strconv.FormatInt(x.migrationID, 10)
	x.SQL = x.upSQL
	if x.Action == "rollback" {
		x.SQL = x.downSQL
	}
	return x, err
}

func approvals(ctx context.Context, q querier, x request) ([]contracts.Approval, error) {
	rows, err := q.Query(ctx, `
		SELECT g.id, g.name, coalesce(u.id, 0), coalesce(u.username::text, ''),
		       CASE WHEN u.id IS NULL THEN 'no_leader'
		            WHEN a.decision = 'approve' THEN 'approved'
		            WHEN a.decision = 'reject' THEN 'rejected'
		            ELSE 'pending' END,
		       coalesce(a.comment, '')
		FROM db_grants dg
		JOIN groups g ON g.id = dg.group_id
		LEFT JOIN group_leaders gl ON gl.group_id = g.id
		LEFT JOIN users u ON u.id = gl.user_id AND u.is_active
		LEFT JOIN migration_approvals a ON a.request_id = $2 AND a.group_id = g.id AND a.leader_id = u.id
		WHERE dg.connection_id = $1
		ORDER BY g.name`, x.connID, x.ID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[contracts.Approval])
}

func evaluate(ctx context.Context, q querier, x *request) error {
	list, err := approvals(ctx, q, *x)
	if err != nil {
		return err
	}
	x.Approvals = list
	pos, err := loadPosition(ctx, q, x.connID)
	if err != nil {
		return err
	}
	x.Validation = validation(*x, pos)
	x.Status = x.stored
	if x.stored != "pending_review" {
		return nil
	}
	if !x.targetMatches || (x.Action == "apply" && x.RevisionID != x.latestRev) {
		x.Status = "stale"
		return nil
	}
	approved := len(list) > 0
	for _, a := range list {
		if a.Status == "rejected" {
			x.Status = "rejected"
			return nil
		}
		approved = approved && a.Status == "approved"
	}
	if approved {
		x.Status = "approved"
	}
	return nil
}

func validation(x request, pos position) []string {
	var out []string
	if !x.targetMatches && !(x.stored == "draft" && x.identity == "") {
		out = append(out, "Konfigurasi target berubah; buat pengajuan baru untuk ditinjau.")
	}
	if err := validateBatchRequest(x); err != nil {
		out = append(out, err.Error())
	}
	if x.Action == "apply" && x.RevisionID != x.latestRev {
		out = append(out, "Revisi sudah diganti; ajukan ulang dengan revisi terbaru.")
	}
	if x.Action != "rollback" && x.stored != "queued" && strings.TrimSpace(x.downSQL) == "" && !x.irrev {
		out = append(out, "Lengkapi SQL down atau tandai irreversible beserta alasannya.")
	}
	if x.Action == "rollback" && x.irrev {
		out = append(out, "Migration irreversible tidak dapat di-rollback.")
	}
	if x.stored != "queued" {
		if msg := pos.check(x.Action, x.migrationID, x.TargetExecutionID); msg != "" {
			out = append(out, msg)
		}
	}
	return out
}

func (s *Service) flags(ctx context.Context, q querier, p reqctx.Principal, x *request) {
	edit := authz.CanUsing(ctx, q, p, x.connID, authz.MigrationExecute) == nil
	valid := len(x.Validation) == 0
	x.CanSubmit = x.Status == "draft" && edit && valid
	x.CanEnqueue = x.Status == "approved" && edit && valid
	for _, a := range x.Approvals {
		if a.LeaderID == p.UserID && (x.Status == "pending_review" || x.Status == "approved" || x.Status == "rejected" || x.Status == "queued") {
			x.CanApprove = true
		}
	}
}

func (s *Service) load(ctx context.Context, q querier, p reqctx.Principal, connID uuid.UUID, rid string, lock bool) (request, error) {
	id, err := parseUUID(rid)
	if err != nil {
		return request{}, err
	}
	query := selectRequest + ` WHERE q.connection_id = $1 AND q.id = $2`
	if lock {
		query += ` FOR UPDATE OF q`
	}
	x, err := scanRequest(q.QueryRow(ctx, query, connID, id))
	if err != nil {
		return x, err
	}
	if err := evaluate(ctx, q, &x); err != nil {
		return x, err
	}
	s.flags(ctx, q, p, &x)
	return x, nil
}

func (s *Service) Requests(ctx context.Context, p reqctx.Principal, connID uuid.UUID) ([]contracts.ChangeRequest, error) {
	rows, err := s.DB.Query(ctx, selectRequest+` WHERE q.connection_id = $1 ORDER BY q.created_at DESC LIMIT 200`, connID)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (request, error) { return scanRequest(r) })
	if err != nil {
		return nil, err
	}
	out := make([]contracts.ChangeRequest, 0, len(list))
	for i := range list {
		if err := evaluate(ctx, s.DB, &list[i]); err != nil {
			return nil, err
		}
		s.flags(ctx, s.DB, p, &list[i])
		out = append(out, list[i].ChangeRequest)
	}
	return out, nil
}

func (s *Service) Request(ctx context.Context, p reqctx.Principal, connID uuid.UUID, rid string) (contracts.ChangeRequest, error) {
	return s.RequestUsing(ctx, s.DB, p, connID, rid)
}

func (s *Service) RequestTx(ctx context.Context, tx pgx.Tx, p reqctx.Principal, connID uuid.UUID, rid string) (contracts.ChangeRequest, error) {
	return s.RequestUsing(ctx, tx, p, connID, rid)
}

func (s *Service) RequestUsing(ctx context.Context, q querier, p reqctx.Principal, connID uuid.UUID, rid string) (contracts.ChangeRequest, error) {
	x, err := s.load(ctx, q, p, connID, rid, false)
	return x.ChangeRequest, err
}

func (s *Service) Propose(ctx context.Context, p reqctx.Principal, connID uuid.UUID, in contracts.ChangeInput) (contracts.ChangeRequest, error) {
	var out contracts.ChangeRequest
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		var err error
		out, err = s.ProposeUsing(ctx, tx, p, connID, in)
		return err
	})
	return out, err
}

func (s *Service) ProposeUsing(ctx context.Context, tx pgx.Tx, p reqctx.Principal, connID uuid.UUID, in contracts.ChangeInput) (contracts.ChangeRequest, error) {
	if err := authz.CanUsing(ctx, tx, p, connID, authz.MigrationExecute); err != nil {
		return contracts.ChangeRequest{}, err
	}
	mid, err := parseMigrationID(in.MigrationID)
	if err != nil {
		return contracts.ChangeRequest{}, err
	}
	rev, err := parseUUID(in.RevisionID)
	if err != nil {
		return contracts.ChangeRequest{}, migration.ValidationError("Revisi tidak valid.")
	}
	if in.Action != "apply" && in.Action != "rollback" && in.Action != "forward" {
		return contracts.ChangeRequest{}, migration.ValidationError("Aksi tidak valid.")
	}
	if in.Mode == "" {
		in.Mode = "normal"
	}
	if in.Mode != "normal" && in.Mode != "batch" {
		return contracts.ChangeRequest{}, migration.ValidationError("Mode tidak valid.")
	}
	if in.Mode != "batch" {
		in.Mode, in.BatchSize, in.CursorColumn = "normal", 0, ""
	}
	if utf8.RuneCountInString(in.Scope) > 4000 || len(in.CursorColumn) > 200 {
		return contracts.ChangeRequest{}, migration.ValidationError("Cakupan atau kolom cursor terlalu panjang.")
	}
	var target *uuid.UUID
	var originalIdentity, originalServer string
	var id string
	err = func() error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM migration_revisions r JOIN migrations m ON m.id = r.migration_id
			WHERE r.id = $1 AND m.id = $2 AND m.connection_id = $3)`, rev, mid, connID).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return migration.ErrNotFound
		}
		if in.Action != "apply" {
			t, err := parseUUID(in.TargetExecutionID)
			if err != nil {
				return migration.ValidationError("Rollback/forward memerlukan execution target.")
			}
			var want string
			var priorJob *string
			err = tx.QueryRow(ctx, `SELECT revision_id::text,target_identity,target_server,job_id FROM migration_executions WHERE id = $1 AND migration_id = $2 AND connection_id = $3`, t, mid, connID).Scan(&want, &originalIdentity, &originalServer, &priorJob)
			if errors.Is(err, pgx.ErrNoRows) {
				return migration.ErrNotFound
			}
			if err != nil {
				return err
			}
			if priorJob != nil && originalIdentity == "" {
				return conflict("Identitas execution lama belum terverifikasi.")
			}
			if want != rev.String() {
				return migration.ValidationError("Revisi harus sama dengan revisi execution target.")
			}
			target = &t
			// Both directions use the batch configuration of the referenced attempt.
			err = tx.QueryRow(ctx, `SELECT j.plan->>'Mode',coalesce((j.plan->>'BatchSize')::int,0),coalesce(j.plan->>'CursorColumn',''),coalesce(j.plan->>'Scope','')
      FROM migration_executions e JOIN jobs j ON j.id::text=e.job_id WHERE e.id=$1 AND j.plan->>'Mode'='batch'`, t).Scan(&in.Mode, &in.BatchSize, &in.CursorColumn, &in.Scope)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
		}
		var up, down string
		if err := tx.QueryRow(ctx, `SELECT sql_up,sql_down FROM migration_revisions WHERE id=$1`, rev).Scan(&up, &down); err != nil {
			return err
		}
		x := request{ChangeRequest: contracts.ChangeRequest{ChangeInput: in}, upSQL: up, downSQL: down}
		if err := tx.QueryRow(ctx, `SELECT `+dbconn.IdentityFingerprint+` FROM db_connections dc WHERE id=$1 AND deleted_at IS NULL FOR SHARE`, connID).Scan(&x.identity); err != nil {
			return err
		}
		if originalIdentity != "" && x.identity != originalIdentity {
			return conflict("Identitas target berbeda dari execution asal.")
		}
		x.SQL = up
		if in.Action == "rollback" {
			x.SQL = down
		}
		if err := validateBatchRequest(x); err != nil {
			return err
		}
		if err := s.validateTarget(ctx, tx, connID, &x, true); err != nil {
			return err
		}
		if originalServer != "" && originalServer != x.physical {
			return conflict("Server target berbeda dari execution asal.")
		}
		return tx.QueryRow(ctx, `
   INSERT INTO migration_requests (connection_id, migration_id, revision_id, action, target_execution_id, mode, batch_size, cursor_column, scope, created_by, target_fingerprint, target_identity,target_server)
 SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9,$10, `+targetFingerprint+`, $11,$12 FROM db_connections dc WHERE dc.id=$1 RETURNING id::text`,
			connID, mid, rev, in.Action, target, in.Mode, in.BatchSize, strings.TrimSpace(in.CursorColumn), in.Scope, p.UserID, x.identity, x.physical).Scan(&id)
	}()
	if err != nil {
		return contracts.ChangeRequest{}, err
	}
	return s.RequestUsing(ctx, tx, p, connID, id)
}

func (s *Service) Submit(ctx context.Context, p reqctx.Principal, connID uuid.UUID, rid string) (contracts.ChangeRequest, error) {
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		x, err := s.load(ctx, tx, p, connID, rid, true)
		if err != nil {
			return err
		}
		if x.Status != "draft" {
			return conflict("Pengajuan sudah dikirim untuk review.")
		}
		if len(x.Validation) > 0 {
			return migration.ValidationError(strings.Join(x.Validation, " "))
		}
		if err := authz.CanUsing(ctx, tx, p, connID, authz.MigrationExecute); err != nil {
			return err
		}
		if x.identity == "" {
			if err := tx.QueryRow(ctx, `SELECT `+dbconn.IdentityFingerprint+`,`+targetFingerprint+` FROM db_connections dc WHERE id=$1 AND deleted_at IS NULL`, connID).Scan(&x.identity, &x.fingerprint); err != nil {
				return err
			}
		}
		if err := s.validateTarget(ctx, tx, connID, &x, true); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE migration_requests SET status = 'pending_review', submitted_at = now(), target_identity=$2,target_fingerprint=$3,target_server=$4 WHERE id = $1`, x.ID, x.identity, x.fingerprint, x.physical)
		return err
	})
	if err != nil {
		return contracts.ChangeRequest{}, err
	}
	return s.Request(ctx, p, connID, rid)
}

func (s *Service) Review(ctx context.Context, p reqctx.Principal, connID uuid.UUID, rid string, f contracts.Review) (contracts.ChangeRequest, error) {
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		x, err := s.load(ctx, tx, p, connID, rid, true)
		if err != nil {
			return err
		}
		if (x.stored != "pending_review" && x.stored != "queued") || x.Status == "stale" {
			return conflict("Pengajuan tidak sedang menunggu review.")
		}
		if f.Decision != "approve" && f.Decision != "reject" || utf8.RuneCountInString(f.Comment) > 4000 {
			return migration.ValidationError("Keputusan review tidak valid.")
		}
		if f.Decision == "approve" {
			if !x.targetMatches {
				return conflict("Identitas atau konfigurasi target tidak lagi sah.")
			}
			if err := validateBatchRequest(x); err != nil {
				return err
			}
			if x.stored != "queued" && len(x.Validation) > 0 {
				return migration.ValidationError(strings.Join(x.Validation, " "))
			}
			if err := s.validateTarget(ctx, tx, connID, &x, true); err != nil {
				return err
			}
		}
		led := 0
		for _, a := range x.Approvals {
			if a.LeaderID != p.UserID {
				continue
			}
			led++
			if _, err := tx.Exec(ctx, `
				INSERT INTO migration_approvals (request_id, group_id, leader_id, decision, comment) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (request_id, group_id, leader_id) DO UPDATE SET decision = EXCLUDED.decision, comment = EXCLUDED.comment, decided_at = now()`,
				x.ID, a.GroupID, p.UserID, f.Decision, f.Comment); err != nil {
				return err
			}
		}
		if led == 0 {
			return forbidden("Hanya leader group yang memiliki akses ke database ini yang dapat meninjau.")
		}
		return nil
	})
	if err != nil {
		return contracts.ChangeRequest{}, err
	}
	return s.Request(ctx, p, connID, rid)
}

func plan(x request, actor int64) Plan {
	return Plan{RequestID: x.ID, ConnectionID: x.connID, MigrationID: x.MigrationID, RevisionID: x.RevisionID,
		Action: x.Action, TargetExecutionID: x.TargetExecutionID, Mode: x.Mode, BatchSize: x.BatchSize,
		CursorColumn: x.CursorColumn, Scope: x.Scope, SQL: x.SQL, ActorID: actor, TargetFingerprint: x.fingerprint, TargetIdentity: x.identity, TargetServer: x.physical}
}

func (s *Service) Enqueue(ctx context.Context, p reqctx.Principal, connID uuid.UUID, rid, key string) (contracts.Job, error) {
	if s.Queue == nil {
		return contracts.Job{}, &contracts.Error{Status: 503, Code: "module_unavailable", Message: "Layanan queue dan worker belum tersedia."}
	}
	if !s.canEdit(ctx, p, connID) {
		return contracts.Job{}, forbidden("Izin Edit diperlukan.")
	}
	var job contracts.Job
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM db_connections WHERE id = $1 FOR UPDATE`, connID); err != nil {
			return err
		}
		x, err := s.load(ctx, tx, p, connID, rid, true)
		if err != nil {
			return err
		}
		if x.stored == "queued" {
			job, err = s.Queue.Enqueue(ctx, tx, plan(x, *x.queuedBy), key)
			return err
		}
		if x.Status != "approved" {
			return conflict("Pengajuan belum disetujui seluruh leader yang diperlukan.")
		}
		if len(x.Validation) > 0 {
			return conflict(strings.Join(x.Validation, " "))
		}
		if err := s.validateTarget(ctx, tx, connID, &x, true); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE migration_requests SET status = 'queued', queued_by = $2, queued_at = now() WHERE id = $1`, x.ID, p.UserID); err != nil {
			return err
		}
		job, err = s.Queue.Enqueue(ctx, tx, plan(x, p.UserID), key)
		return err
	})
	return job, err
}

func (s *Service) Prepare(ctx context.Context, requestID string) (Plan, error) {
	id, err := parseUUID(requestID)
	if err != nil {
		return Plan{}, err
	}
	x, err := scanRequest(s.DB.QueryRow(ctx, selectRequest+` WHERE q.id = $1`, id))
	if err != nil {
		return Plan{}, err
	}
	if x.stored != "queued" || x.queuedBy == nil {
		return Plan{}, conflict("Pengajuan belum masuk queue.")
	}
	x.stored = "pending_review"
	if err := evaluate(ctx, s.DB, &x); err != nil {
		return Plan{}, err
	}
	if x.Status != "approved" {
		return Plan{}, conflict("Approval tidak lagi sah (revisi, leader, atau grant berubah).")
	}
	if err := s.validateTarget(ctx, s.DB, x.connID, &x, false); err != nil {
		return Plan{}, err
	}
	if len(x.Validation) > 0 {
		return Plan{}, conflict(strings.Join(x.Validation, " "))
	}
	actor := reqctx.Principal{UserID: *x.queuedBy}
	if err := authz.CanUsing(ctx, s.DB, actor, x.connID, authz.MigrationExecute); err != nil {
		if errors.Is(err, authz.ErrForbidden) {
			return Plan{}, forbidden("Pengguna yang mengirim ke queue tidak lagi memiliki izin eksekusi.")
		}
		return Plan{}, err
	}
	return plan(x, actor.UserID), nil
}

func (s *Service) BeginAttempt(ctx context.Context, pl Plan, jobID string, attemptNo int) (string, error) {
	return beginAttempt(ctx, s.DB, pl, jobID, attemptNo)
}

func beginAttempt(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, pl Plan, jobID string, attemptNo int) (string, error) {
	var reverts *string
	if pl.Action == "rollback" {
		reverts = &pl.TargetExecutionID
	}
	var id string
	err := q.QueryRow(ctx, `
		INSERT INTO migration_executions (connection_id, migration_id, revision_id, request_id, job_id, action, attempt, status, sql, reverts_execution_id, actor_id, target_identity,target_server,sql_dispatch_tracked)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'running',$8,$9,$10,$11,$12,
		        (SELECT dbms = 'postgresql' FROM db_connections WHERE id = $1)) RETURNING id::text`,
		pl.ConnectionID, pl.MigrationID, pl.RevisionID, pl.RequestID, jobID, pl.Action, attemptNo, pl.SQL, reverts, pl.ActorID, pl.TargetIdentity, pl.TargetServer).Scan(&id)
	return id, err
}

var finalStatus = map[string]bool{"success": true, "failed": true, "commit_unknown": true, "partial": true, "partially_reverted": true}

func (s *Service) FinishAttempt(ctx context.Context, executionID, status, errMsg string) error {
	return finishAttempt(ctx, s.DB, executionID, status, errMsg)
}

func finishAttempt(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}, executionID, status, errMsg string) error {
	if !finalStatus[status] {
		return fmt.Errorf("workflow: status attempt tidak dikenal: %q", status)
	}
	id, err := parseUUID(executionID)
	if err != nil {
		return err
	}
	var msg *string
	if errMsg != "" {
		msg = &errMsg
	}
	tag, err := q.Exec(ctx, `
		UPDATE migration_executions SET status = $2, error_message = $3, finished_at = $4
		WHERE id = $1 AND status NOT IN ('success','failed') AND status <> $2`, id, status, msg, time.Now())
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var current string
	if err := q.QueryRow(ctx, `SELECT status FROM migration_executions WHERE id = $1`, id).Scan(&current); err != nil {
		return err
	}
	if current != status {
		return conflict(fmt.Sprintf("Attempt sudah berstatus %s.", current))
	}
	return nil
}
