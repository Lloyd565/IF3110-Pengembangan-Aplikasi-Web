package dbconn

import (
	"context"
	"database/sql"
	"errors"
	"github.com/go-sql-driver/mysql"
	"strings"
)

const MariaIdentityTable = "_platform_target_identity"

func IsMariaControlTable(name string) bool {
	switch strings.ToLower(name) {
	case MariaIdentityTable, "_platform_migrations", "_platform_workflow_runs":
		return true
	}
	return false
}

var ErrTargetIdentity = errors.New("Identitas database target tidak cocok.")

func PostgresIdentity(ctx context.Context, q Querier) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT 'pg:'||s.system_identifier::text||':'||d.oid::text
 FROM pg_catalog.pg_control_system() s JOIN pg_catalog.pg_database d ON d.datname=pg_catalog.current_database()`).Scan(&id)
	return id, err
}

func MariaIdentity(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT CONCAT('maria:',@@server_uid,':',DATABASE(),':',identity_token) FROM `+MariaIdentityTable+` WHERE id=1`).Scan(&id)
	return id, err
}

func (t *Target) PhysicalIdentity(ctx context.Context) (string, error) {
	if t.DBMS == PostgreSQL && t.Postgres != nil {
		return PostgresIdentity(ctx, t.Postgres)
	}
	if t.DBMS == MariaDB && t.Maria != nil {
		identity, err := MariaIdentity(ctx, t.Maria)
		if err == nil {
			return identity, nil
		}
		var dbError *mysql.MySQLError
		if !errors.Is(err, sql.ErrNoRows) && (!errors.As(err, &dbError) || dbError.Number != 1146) {
			return "", err
		}
		if _, err := t.Maria.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+MariaIdentityTable+`(
   id TINYINT PRIMARY KEY CHECK(id=1),identity_token CHAR(36) NOT NULL) ENGINE=InnoDB`); err != nil {
			return "", err
		}
		if _, err := t.Maria.ExecContext(ctx, `INSERT INTO `+MariaIdentityTable+`(id,identity_token) VALUES(1,UUID())
   ON DUPLICATE KEY UPDATE id=id`); err != nil {
			return "", err
		}
		return MariaIdentity(ctx, t.Maria)
	}
	return "", ErrTargetIdentity
}
