package dbconn

import (
	"context"
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
	cfg, err := pgxpool.ParseConfig(p.url())
	if err != nil {
		return nil, errors.New("dbconn: parameter koneksi tidak valid")
	}
	cfg.MaxConns = 4
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("dbconn: parameter koneksi tidak valid")
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dbconn: tidak bisa terhubung ke %s:%d: %w", p.Host, p.Port, err)
	}
	return pool, nil
}

// Check: untuk tombol "Test connection". Tidak menyimpan apa pun.
func Check(ctx context.Context, p Params) error {
	pool, err := Connect(ctx, p)
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

	mu    sync.Mutex
	pools map[uuid.UUID]*pgxpool.Pool
	dev   *pgxpool.Pool
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

func (r *Registry) Pool(ctx context.Context, connID uuid.UUID) (*pgxpool.Pool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p := r.pools[connID]; p != nil {
		return p, nil
	}

	var p Params
	var enc []byte
	err := r.DB.QueryRow(ctx, `
		SELECT host, port, dbname, username, password_enc, sslmode
		FROM db_connections WHERE id = $1 AND deleted_at IS NULL`, connID,
	).Scan(&p.Host, &p.Port, &p.DBName, &p.Username, &enc, &p.SSLMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return r.devFallback()
	}
	if err != nil {
		return nil, fmt.Errorf("dbconn: baca koneksi: %w", err)
	}

	key, err := r.key()
	if err != nil {
		return nil, err
	}
	if p.Password, err = Decrypt(key, connID, enc); err != nil {
		return nil, err
	}
	pool, err := Connect(ctx, p)
	if err != nil {
		return nil, err
	}
	if r.pools == nil {
		r.pools = map[uuid.UUID]*pgxpool.Pool{}
	}
	r.pools[connID] = pool
	return pool, nil
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
		go p.Close() 
	}
}
