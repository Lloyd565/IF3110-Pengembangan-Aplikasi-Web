// Package admin: operasi Platform Admin (user, group, koneksi, grant).
// Pemeriksaan "pemanggil adalah admin" dilakukan di lapisan handler (requireAdmin);
package admin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/auth"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/dbconn"
	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/user"
)

var (
	ErrInvalid  = errors.New("admin: input tidak valid")
	ErrNotFound = errors.New("admin: data tidak ditemukan")
	ErrSelf     = errors.New("admin: aksi ini tidak boleh dilakukan pada akun sendiri")
)

const minPasswordLen = 8

type Service struct {
	DB       *pgxpool.Pool
	Users    *user.Repo
	Sessions *auth.Sessions
	Conns    *dbconn.Registry
	Hash     func(password string) (string, error) 
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

func pgCode(err error) string {
	var e *pgconn.PgError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func (s *Service) audit(ctx context.Context, actor int64, action, target string) {
	if _, err := s.DB.Exec(ctx,
		`INSERT INTO audit_log (actor_id, action, target) VALUES ($1, $2, $3)`, actor, action, target); err != nil {
		log.Printf("audit %s: %v", action, err)
	}
}

func (s *Service) ListUsers(ctx context.Context) ([]user.User, error) { return s.Users.List(ctx) }

func checkPassword(pw string) error {
	if len(pw) < minPasswordLen {
		return invalid("password minimal %d karakter", minPasswordLen)
	}
	return nil
}

func (s *Service) CreateUser(ctx context.Context, actor int64, username, password string, admin bool) (int64, error) {
	username = strings.TrimSpace(username)
	if username == "" || len(username) > 64 {
		return 0, invalid("username wajib diisi (maks 64 karakter)")
	}
	if err := checkPassword(password); err != nil {
		return 0, err
	}
	hash, err := s.Hash(password)
	if err != nil {
		return 0, err
	}
	id, err := s.Users.Create(ctx, username, hash, admin)
	if errors.Is(err, user.ErrUsernameTaken) {
		return 0, invalid("username sudah dipakai")
	}
	if err != nil {
		return 0, err
	}
	s.audit(ctx, actor, "user.create", fmt.Sprintf("user:%d", id))
	return id, nil
}

// SetUserActive: menonaktifkan user langsung mencabut semua session-nya di backend.
func (s *Service) SetUserActive(ctx context.Context, actor, id int64, active bool) error {
	if !active && id == actor {
		return ErrSelf
	}
	if err := s.Users.SetActive(ctx, id, active); err != nil {
		return mapUserErr(err)
	}
	if !active {
		if err := s.Sessions.RevokeAllForUser(ctx, id); err != nil {
			return err
		}
	}
	s.audit(ctx, actor, fmt.Sprintf("user.active=%t", active), fmt.Sprintf("user:%d", id))
	return nil
}

func (s *Service) ResetPassword(ctx context.Context, actor, id int64, newPassword string) error {
	if err := checkPassword(newPassword); err != nil {
		return err
	}
	hash, err := s.Hash(newPassword)
	if err != nil {
		return err
	}
	if err := s.Users.SetPasswordHash(ctx, id, hash); err != nil {
		return mapUserErr(err)
	}
	if err := s.Sessions.RevokeAllForUser(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, actor, "user.reset_password", fmt.Sprintf("user:%d", id))
	return nil
}

// SetAdmin: privilege naik -> session user itu dirotasi pada request berikutnya.
func (s *Service) SetAdmin(ctx context.Context, actor, id int64, admin bool) error {
	if !admin && id == actor {
		return ErrSelf
	}
	if err := s.Users.SetAdmin(ctx, id, admin); err != nil {
		return mapUserErr(err)
	}
	if admin {
		if err := s.Sessions.MarkForceRotate(ctx, id); err != nil {
			return err
		}
	}
	s.audit(ctx, actor, fmt.Sprintf("user.admin=%t", admin), fmt.Sprintf("user:%d", id))
	return nil
}

func mapUserErr(err error) error {
	if errors.Is(err, user.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

type Group struct {
	ID          int64
	Name        string
	Description string
	Members     int
}

type Member struct {
	UserID   int64
	Username string
}

func (s *Service) CreateGroup(ctx context.Context, actor int64, name, description string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, invalid("nama group wajib diisi")
	}
	var id int64
	err := s.DB.QueryRow(ctx,
		`INSERT INTO groups (name, description) VALUES ($1, $2) RETURNING id`, name, description).Scan(&id)
	if pgCode(err) == "23505" {
		return 0, invalid("nama group sudah dipakai")
	}
	if err != nil {
		return 0, err
	}
	s.audit(ctx, actor, "group.create", fmt.Sprintf("group:%d", id))
	return id, nil
}

func (s *Service) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT g.id, g.name, g.description, count(m.user_id)
		FROM groups g LEFT JOIN group_members m ON m.group_id = g.id
		GROUP BY g.id ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Description, &g.Members); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Service) Members(ctx context.Context, groupID int64) ([]Member, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT u.id, u.username FROM group_members m JOIN users u ON u.id = m.user_id
		WHERE m.group_id = $1 ORDER BY u.username`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.UserID, &m.Username); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMember: jika group sudah punya grant, privilege user naik -> force_rotate.
func (s *Service) AddMember(ctx context.Context, actor, groupID, userID int64) error {
	_, err := s.DB.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, groupID, userID)
	if pgCode(err) == "23503" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var hasGrant bool
	if err := s.DB.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM db_grants WHERE group_id = $1)`, groupID).Scan(&hasGrant); err != nil {
		return err
	}
	if hasGrant {
		if err := s.Sessions.MarkForceRotate(ctx, userID); err != nil {
			return err
		}
	}
	s.audit(ctx, actor, "group.add_member", fmt.Sprintf("group:%d user:%d", groupID, userID))
	return nil
}

// RemoveMember: tidak perlu cabut session; authz.Can membaca grant tiap request.
func (s *Service) RemoveMember(ctx context.Context, actor, groupID, userID int64) error {
	if _, err := s.DB.Exec(ctx,
		`DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, userID); err != nil {
		return err
	}
	s.audit(ctx, actor, "group.remove_member", fmt.Sprintf("group:%d user:%d", groupID, userID))
	return nil
}

// ---- Koneksi ----

type Connection struct {
	ID       uuid.UUID
	Name     string
	Host     string
	Port     int
	DBName   string
	Username string
	SSLMode  string // password sengaja tidak pernah ikut keluar
}

var sslModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true, "require": true, "verify-ca": true, "verify-full": true,
}

func normalize(name string, p *dbconn.Params) error {
	if strings.TrimSpace(name) == "" {
		return invalid("nama koneksi wajib diisi")
	}
	if strings.TrimSpace(p.Host) == "" || strings.TrimSpace(p.DBName) == "" || strings.TrimSpace(p.Username) == "" {
		return invalid("host, nama database, dan username wajib diisi")
	}
	if p.Port < 1 || p.Port > 65535 {
		return invalid("port harus 1-65535")
	}
	if p.SSLMode == "" {
		p.SSLMode = "disable"
	}
	if !sslModes[p.SSLMode] {
		return invalid("sslmode tidak dikenal")
	}
	return nil
}

func (s *Service) CreateConnection(ctx context.Context, actor int64, name string, p dbconn.Params) (uuid.UUID, error) {
	if err := normalize(name, &p); err != nil {
		return uuid.Nil, err
	}
	if p.Password == "" {
		return uuid.Nil, invalid("password koneksi wajib diisi")
	}
	id := uuid.New()
	enc, err := s.Conns.Seal(id, p.Password)
	if err != nil {
		return uuid.Nil, err
	}
	_, err = s.DB.Exec(ctx, `
		INSERT INTO db_connections (id, name, host, port, dbname, username, password_enc, sslmode, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		id, name, p.Host, p.Port, p.DBName, p.Username, enc, p.SSLMode, actor)
	if pgCode(err) == "23505" {
		return uuid.Nil, invalid("nama koneksi sudah dipakai")
	}
	if err != nil {
		return uuid.Nil, err
	}
	s.audit(ctx, actor, "connection.create", "connection:"+id.String())
	return id, nil
}

// UpdateConnection: password kosong = pertahankan password lama.
func (s *Service) UpdateConnection(ctx context.Context, actor int64, id uuid.UUID, name string, p dbconn.Params) error {
	if err := normalize(name, &p); err != nil {
		return err
	}
	var tag pgconn.CommandTag
	var err error
	if p.Password == "" {
		tag, err = s.DB.Exec(ctx, `
			UPDATE db_connections SET name=$2, host=$3, port=$4, dbname=$5, username=$6, sslmode=$7, updated_at=now()
			WHERE id=$1 AND deleted_at IS NULL`, id, name, p.Host, p.Port, p.DBName, p.Username, p.SSLMode)
	} else {
		var enc []byte
		if enc, err = s.Conns.Seal(id, p.Password); err != nil {
			return err
		}
		tag, err = s.DB.Exec(ctx, `
			UPDATE db_connections SET name=$2, host=$3, port=$4, dbname=$5, username=$6, sslmode=$7,
			       password_enc=$8, updated_at=now()
			WHERE id=$1 AND deleted_at IS NULL`, id, name, p.Host, p.Port, p.DBName, p.Username, p.SSLMode, enc)
	}
	if pgCode(err) == "23505" {
		return invalid("nama koneksi sudah dipakai")
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Conns.Invalidate(id)
	s.audit(ctx, actor, "connection.update", "connection:"+id.String())
	return nil
}

// DeleteConnection: soft delete agar riwayat migration tetap ada.
func (s *Service) DeleteConnection(ctx context.Context, actor int64, id uuid.UUID) error {
	tag, err := s.DB.Exec(ctx,
		`UPDATE db_connections SET deleted_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	s.Conns.Invalidate(id)
	s.audit(ctx, actor, "connection.delete", "connection:"+id.String())
	return nil
}

func (s *Service) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT id, name, host, port, dbname, username, sslmode
		FROM db_connections WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Connection
	for rows.Next() {
		var c Connection
		if err := rows.Scan(&c.ID, &c.Name, &c.Host, &c.Port, &c.DBName, &c.Username, &c.SSLMode); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TestConnection: untuk tombol "Test connection" pada form.
func (s *Service) TestConnection(ctx context.Context, p dbconn.Params) error {
	if err := normalize("x", &p); err != nil {
		return err
	}
	return dbconn.Check(ctx, p)
}

type Grant struct {
	GroupID        int64
	GroupName      string
	ConnectionID   uuid.UUID
	ConnectionName string
	Level          string // "view" atau "edit"
}

// SetGrant: grant baru atau level naik -> semua anggota group di-force_rotate.
func (s *Service) SetGrant(ctx context.Context, actor, groupID int64, connID uuid.UUID, level string) error {
	if level != "view" && level != "edit" {
		return invalid("level harus view atau edit")
	}
	var live bool
	if err := s.DB.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM db_connections WHERE id = $1 AND deleted_at IS NULL)`, connID).Scan(&live); err != nil {
		return err
	}
	if !live {
		return ErrNotFound
	}

	var old string
	err := s.DB.QueryRow(ctx,
		`SELECT level FROM db_grants WHERE group_id = $1 AND connection_id = $2`, groupID, connID).Scan(&old)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	_, err = s.DB.Exec(ctx, `
		INSERT INTO db_grants (group_id, connection_id, level) VALUES ($1, $2, $3)
		ON CONFLICT (group_id, connection_id) DO UPDATE SET level = EXCLUDED.level`, groupID, connID, level)
	if pgCode(err) == "23503" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if old == "" || (old == "view" && level == "edit") {
		rows, err := s.DB.Query(ctx, `SELECT user_id FROM group_members WHERE group_id = $1`, groupID)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := s.Sessions.MarkForceRotate(ctx, id); err != nil {
				return err
			}
		}
	}
	s.audit(ctx, actor, "grant.set="+level, fmt.Sprintf("group:%d connection:%s", groupID, connID))
	return nil
}

func (s *Service) RemoveGrant(ctx context.Context, actor, groupID int64, connID uuid.UUID) error {
	if _, err := s.DB.Exec(ctx,
		`DELETE FROM db_grants WHERE group_id = $1 AND connection_id = $2`, groupID, connID); err != nil {
		return err
	}
	s.audit(ctx, actor, "grant.remove", fmt.Sprintf("group:%d connection:%s", groupID, connID))
	return nil
}

func (s *Service) ListGrants(ctx context.Context) ([]Grant, error) {
	rows, err := s.DB.Query(ctx, `
		SELECT g.id, g.name, c.id, c.name, d.level
		FROM db_grants d
		JOIN groups g ON g.id = d.group_id
		JOIN db_connections c ON c.id = d.connection_id AND c.deleted_at IS NULL
		ORDER BY g.name, c.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.GroupID, &g.GroupName, &g.ConnectionID, &g.ConnectionName, &g.Level); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}
