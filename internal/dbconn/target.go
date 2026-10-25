package dbconn

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
)

var mariaVersionPattern = regexp.MustCompile(`(?i)^(?:5\.5\.5-)?(\d+)\.(\d+)\.(\d+).*mariadb`)

func supportedMariaVersion(version string) bool {
	m := mariaVersionPattern.FindStringSubmatch(version)
	if len(m) != 4 {
		return false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	return major > 10 || (major == 10 && (minor > 6 || (minor == 6 && patch >= 1)))
}

type DBMS string

const (
	PostgreSQL DBMS = "postgresql"
	MariaDB    DBMS = "mariadb"
)

func NormalizeDBMS(value DBMS) (DBMS, error) {
	if value == "" {
		return PostgreSQL, nil
	}
	if value != PostgreSQL && value != MariaDB {
		return "", errors.New("dbconn: DBMS harus postgresql atau mariadb")
	}
	return value, nil
}

type Target struct {
	DBMS     DBMS
	Database string
	Postgres *pgxpool.Pool
	Maria    *sql.DB
}

func (t *Target) Close() {
	if t.Postgres != nil {
		t.Postgres.Close()
	}
	if t.Maria != nil {
		t.Maria.Close()
	}
}

func (t *Target) Ping(ctx context.Context) error {
	if t.DBMS == PostgreSQL && t.Postgres != nil {
		return t.Postgres.Ping(ctx)
	}
	if t.DBMS == MariaDB && t.Maria != nil {
		return t.Maria.PingContext(ctx)
	}
	return errors.New("dbconn: target tidak valid")
}

func mariaConfig(p Params) (*mysql.Config, error) {
	cfg := mysql.NewConfig()
	cfg.Net, cfg.Addr = "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port))
	cfg.User, cfg.Passwd, cfg.DBName = p.Username, p.Password, p.DBName
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 65*time.Second, 65*time.Second
	cfg.MultiStatements = false
	cfg.AllowNativePasswords = true
	switch p.SSLMode {
	case "", "disable":
		cfg.TLSConfig = "false"
	case "require":
		cfg.TLSConfig = "skip-verify"
	case "verify-full":
		cfg.TLSConfig = "true"
	default:
		return nil, errors.New("dbconn: MariaDB mendukung SSL disable, require, atau verify-full")
	}
	cfg.Params = map[string]string{"sql_mode": "'STRICT_TRANS_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'", "default_storage_engine": "'InnoDB'"}
	return cfg, nil
}

func ConnectTarget(ctx context.Context, p Params) (*Target, error) {
	kind, err := NormalizeDBMS(p.DBMS)
	if err != nil {
		return nil, err
	}
	if kind == PostgreSQL {
		pool, err := Connect(ctx, p)
		if err != nil {
			return nil, err
		}
		return &Target{DBMS: kind, Database: p.DBName, Postgres: pool}, nil
	}
	cfg, err := mariaConfig(p)
	if err != nil {
		return nil, err
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("dbconn: parameter MariaDB tidak valid")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	t := &Target{DBMS: kind, Database: p.DBName, Maria: db}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := t.Ping(pingCtx); err != nil {
		t.Close()
		return nil, errors.New("dbconn: MariaDB tidak dapat dihubungi; periksa host, port, kredensial, dan TLS")
	}
	var version string
	if err := db.QueryRowContext(pingCtx, "SELECT VERSION()").Scan(&version); err != nil {
		t.Close()
		return nil, errors.New("dbconn: versi MariaDB tidak dapat dibaca")
	}
	if !supportedMariaVersion(version) {
		t.Close()
		return nil, errors.New("dbconn: target harus MariaDB versi 10.6 atau lebih baru")
	}
	return t, nil
}
