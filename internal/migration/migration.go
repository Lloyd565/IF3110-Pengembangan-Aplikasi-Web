package migration

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusDraft   = "draft"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"

	maxNameLen  = 200
	maxSQLBytes = 100_000
)

var (
	ErrNotFound = errors.New("migration: tidak ditemukan")
	ErrNotDraft = errors.New("migration: hanya draft yang bisa diubah")
)

type ValidationError string

func (e ValidationError) Error() string { return string(e) }

type Migration struct {
	ID             int64
	ConnectionID   uuid.UUID
	Version        *int
	Name           string
	SQL            string
	Status         string
	ErrorMessage   *string
	CreatedBy      int64
	CreatedByName  string
	ExecutedBy     *int64
	ExecutedByName *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}

func (m *Migration) IsDraft() bool { return m.Status == StatusDraft }

func Validate(name, sql string) (string, string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return name, sql, ValidationError("Nama migration wajib diisi.")
	case utf8.RuneCountInString(name) > maxNameLen:
		return name, sql, ValidationError("Nama migration maksimal 200 karakter.")
	case strings.TrimSpace(sql) == "":
		return name, sql, ValidationError("SQL migration wajib diisi.")
	case len(sql) > maxSQLBytes:
		return name, sql, ValidationError("SQL migration maksimal 100 KB.")
	}
	return name, sql, nil
}

const selectSQL = `
	SELECT m.id, m.connection_id, m.version, m.name, m.sql, m.status, m.error_message,
	       m.created_by, cu.username::text, m.executed_by, eu.username::text,
	       m.created_at, m.updated_at, m.started_at, m.finished_at
	FROM migrations m
	JOIN users cu ON cu.id = m.created_by
	LEFT JOIN users eu ON eu.id = m.executed_by`

func scan(row pgx.Row) (*Migration, error) {
	var m Migration
	err := row.Scan(&m.ID, &m.ConnectionID, &m.Version, &m.Name, &m.SQL, &m.Status, &m.ErrorMessage,
		&m.CreatedBy, &m.CreatedByName, &m.ExecutedBy, &m.ExecutedByName,
		&m.CreatedAt, &m.UpdatedAt, &m.StartedAt, &m.FinishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &m, err
}

func List(ctx context.Context, db *pgxpool.Pool, connID uuid.UUID) ([]Migration, error) {
	rows, err := db.Query(ctx, selectSQL+`
		WHERE m.connection_id = $1
		ORDER BY m.version DESC NULLS FIRST, m.id DESC`, connID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Migration
	for rows.Next() {
		m, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

func Get(ctx context.Context, db *pgxpool.Pool, connID uuid.UUID, id int64) (*Migration, error) {
	return scan(db.QueryRow(ctx, selectSQL+` WHERE m.connection_id = $1 AND m.id = $2`, connID, id))
}

func CreateDraft(ctx context.Context, db *pgxpool.Pool, connID uuid.UUID, userID int64, name, sql string) (int64, error) {
	name, sql, err := Validate(name, sql)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRow(ctx, `
		INSERT INTO migrations (connection_id, name, sql, created_by)
		VALUES ($1, $2, $3, $4) RETURNING id`, connID, name, sql, userID).Scan(&id)
	return id, err
}

func UpdateDraft(ctx context.Context, db *pgxpool.Pool, connID uuid.UUID, id int64, name, sql string) error {
	name, sql, err := Validate(name, sql)
	if err != nil {
		return err
	}
	tag, err := db.Exec(ctx, `
		UPDATE migrations SET name = $3, sql = $4, updated_at = now()
		WHERE connection_id = $1 AND id = $2 AND status = 'draft'`, connID, id, name, sql)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	if _, err := Get(ctx, db, connID, id); err != nil {
		return err
	}
	return ErrNotDraft
}
