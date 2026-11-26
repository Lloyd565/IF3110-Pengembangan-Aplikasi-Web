package dbconn

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
)

var ErrUnknownConnection = errors.New("dbconn: koneksi tidak terdaftar")

// Params: parameter koneksi ke Target DB. Password hanya ada di memori.
type Params struct {
	Host     string
	Port     int
	DBName   string
	Username string
	Password string
	SSLMode  string
	DBMS     DBMS
}

func (p Params) url() string {
	mode := p.SSLMode
	if mode == "" {
		mode = "disable"
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(p.Username, p.Password),
		Host:     net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
		Path:     "/" + p.DBName,
		RawQuery: url.Values{"sslmode": {mode}}.Encode(),
	}
	return u.String()
}

// Connect membuka pool kecil ke Target DB dan memastikan koneksinya hidup.
func Connect(ctx context.Context, p Params) (*pgxpool.Pool, error) {
	kind, err := NormalizeDBMS(p.DBMS)
	if err != nil || kind != PostgreSQL {
		return nil, errors.New("dbconn: gunakan ConnectTarget untuk koneksi non-PostgreSQL")
	}
	cfg, err := pgxpool.ParseConfig(p.url())
	if err != nil {
		return nil, errors.New("dbconn: parameter koneksi tidak valid")
	}
	cfg.ConnConfig.RuntimeParams["standard_conforming_strings"] = "on"
	cfg.MaxConns = 4
	telemetry.Install(cfg) // seluruh SQL ke target (termasuk read-only explorer) tercatat sebagai log dan span
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("dbconn: parameter koneksi tidak valid")
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, errors.New("dbconn: target PostgreSQL tidak dapat dihubungi; periksa host, port, kredensial, dan TLS")
	}
	return pool, nil
}

// Check: untuk tombol "Test connection". Tidak menyimpan apa pun.
func Check(ctx context.Context, p Params) error {
	pool, err := ConnectTarget(ctx, p)
	if err != nil {
		return err
	}
	pool.Close()
	return nil
}

// Registry menyimpan satu pool per koneksi Target DB. Password dibaca dari
// Platform DB dan didekripsi saat pool pertama kali dibuat.
type Registry struct {
	DB  *pgxpool.Pool
	Key []byte

	keyOnce sync.Once
	keyErr  error

	mu     sync.Mutex
	pools  map[uuid.UUID]*Target
	stamps map[uuid.UUID][32]byte
	dev    *pgxpool.Pool
}

func (r *Registry) key() ([]byte, error) {
	r.keyOnce.Do(func() {
		if len(r.Key) == 0 {
			r.Key, r.keyErr = ParseKey(os.Getenv("ENCRYPTION_KEY"))
		}
	})
	if len(r.Key) == 0 {
		return nil, r.keyErr
	}
	return r.Key, nil
}

// Seal mengenkripsi password koneksi sebelum disimpan di db
func (r *Registry) Seal(connID uuid.UUID, password string) ([]byte, error) {
	key, err := r.key()
	if err != nil {
		return nil, err
	}
	return Encrypt(key, connID, password)
}

func (r *Registry) Unseal(connID uuid.UUID, blob []byte) (string, error) {
	key, err := r.key()
	if err != nil {
		return "", err
	}
	return Decrypt(key, connID, blob)
}

func (r *Registry) Pool(ctx context.Context, connID uuid.UUID) (*pgxpool.Pool, error) {
	target, err := r.Target(ctx, connID)
	if err != nil {
		return nil, err
	}
	if target.DBMS != PostgreSQL {
		return nil, errors.New("dbconn: koneksi ini bukan PostgreSQL")
	}
	return target.Postgres, nil
}

const ConfigurationFingerprint = `encode(sha256(convert_to(jsonb_build_array(dc.dbms,dc.host,dc.port,dc.dbname,dc.username,encode(dc.password_enc,'hex'),dc.sslmode,dc.deleted_at)::text,'UTF8')),'hex')`
const IdentityFingerprint = `encode(sha256(convert_to(jsonb_build_array(dc.id,dc.identity_epoch,dc.dbms,dc.host,dc.port,dc.dbname)::text,'UTF8')),'hex')`

type Querier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (r *Registry) Target(ctx context.Context, connID uuid.UUID) (*Target, error) {
	return r.TargetUsing(ctx, r.DB, connID)
}

func (r *Registry) TargetUsing(ctx context.Context, q Querier, connID uuid.UUID) (*Target, error) {

	var p Params
	var enc []byte
	var configuration, identity string
	err := q.QueryRow(ctx, `
  SELECT host, port, dbname, username, password_enc, sslmode, dbms, `+ConfigurationFingerprint+`, `+IdentityFingerprint+`
  FROM db_connections dc WHERE id = $1 AND deleted_at IS NULL`, connID,
	).Scan(&p.Host, &p.Port, &p.DBName, &p.Username, &enc, &p.SSLMode, &p.DBMS, &configuration, &identity)
	r.mu.Lock()
	defer r.mu.Unlock()
	if errors.Is(err, pgx.ErrNoRows) {
		if old := r.pools[connID]; old != nil {
			delete(r.pools, connID)
			delete(r.stamps, connID)
			go old.Close()
		}
		pool, err := r.devFallback()
		if err != nil {
			return nil, err
		}
		return &Target{DBMS: PostgreSQL, Postgres: pool}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("dbconn: baca koneksi: %w", err)
	}

	serialized, _ := json.Marshal(struct {
		Params     Params
		Ciphertext []byte
		Identity   string
	}{p, enc, identity})
	stamp := sha256.Sum256(serialized)
	if old := r.pools[connID]; old != nil {
		if r.stamps[connID] == stamp {
			return old, nil
		}
		delete(r.pools, connID)
		delete(r.stamps, connID)
		go old.Close()
	}
	key, err := r.key()
	if err != nil {
		return nil, err
	}
	if p.Password, err = Decrypt(key, connID, enc); err != nil {
		return nil, err
	}
	pool, err := ConnectTarget(ctx, p)
	if err != nil {
		return nil, err
	}
	if r.pools == nil {
		r.pools = map[uuid.UUID]*Target{}
	}
	if r.stamps == nil {
		r.stamps = map[uuid.UUID][32]byte{}
	}
	pool.Configuration, pool.Identity = configuration, identity
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("if3110 recovery evidence v1"))
	pool.EvidenceKey = mac.Sum(nil)
	r.pools[connID] = pool
	r.stamps[connID] = stamp
	return pool, nil
}

func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, target := range r.pools {
		target.Close()
		delete(r.pools, id)
		delete(r.stamps, id)
	}
	if r.dev != nil {
		r.dev.Close()
		r.dev = nil
	}
}

func (r *Registry) devFallback() (*pgxpool.Pool, error) {
	u := os.Getenv("DEV_TARGET_DATABASE_URL")
	if u == "" {
		return nil, ErrUnknownConnection
	}
	if r.dev == nil {
		pool, err := pgxpool.New(context.Background(), u)
		if err != nil {
			return nil, err
		}
		r.dev = pool
	}
	return r.dev, nil
}

// Invalidate menutup pool lama setelah koneksi diubah atau dihapus.
func (r *Registry) Invalidate(connID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p := r.pools[connID]; p != nil {
		delete(r.pools, connID)
		delete(r.stamps, connID)
		go p.Close()
	}
}
