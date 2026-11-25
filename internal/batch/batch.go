// Package batch menjalankan SQL migration di Target PostgreSQL untuk worker: mode biasa (satu
// transaksi) dan mode batch (satu transaksi per batch). Semua bukti commit disimpan di tabel
// kontrol pada Target DB (schema _platform) dalam transaksi yang sama dengan perubahan datanya,
// sehingga hasil commit yang belum pasti selalu bisa dipastikan dengan membaca target.
package batch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/internal/telemetry"
)

// lockKey sama dengan kunci lock M1, jadi eksekusi lama (satu transaksi) dan worker saling menunggu.
// Advisory lock berlaku per database fisik, sehingga dua konfigurasi koneksi ke database yang
// sama tidak bisa menghindari lock.
const lockKey = `hashtext('if3110.migration')`

var (
	ErrBusy          = errors.New("batch: migration lain sedang berjalan di database ini")
	ErrCommitUnknown = errors.New("batch: hasil commit belum pasti dan bukti di target tidak bisa dibaca")
	ErrRolledBack    = errors.New("batch: commit tidak terjadi di target")
	ErrUnsafe        = errors.New("batch: pembatalan tidak aman")
	ErrInvalidSpec   = errors.New("batch: spesifikasi tidak valid")
)

var ident = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	OpUp   = "up"
	OpDown = "down"
)

// Hooks hanya untuk failure injection pada test; nil pada produksi.
type Hooks struct {
	BeforeCommit func(op string, batchNo int) error
	AfterCommit  func(op string, batchNo int) error
}

// Metrics diisi telemetry.Metrics; nil berarti tidak ada metrics.
type Metrics interface {
	LockWait(time.Duration)
	BatchCommitted(rows int64)
}

type Runner struct {
	Pool             *pgxpool.Pool
	LockWait         time.Duration // batas menunggu lock database
	StatementTimeout time.Duration // batas satu SQL (terpisah dari timeout HTTP)
	Hooks            Hooks
	Metrics          Metrics
	ReconcileWait    time.Duration // batas membaca bukti di target setelah jawaban commit hilang
}

func (r *Runner) reconcileWait() time.Duration {
	if r.ReconcileWait > 0 {
		return r.ReconcileWait
	}
	return 30 * time.Second
}

// Spec mengidentifikasi satu pekerjaan. RootID stabil lintas retry, resume, dan kompensasi,
// sehingga bukti di target tidak pernah tertukar antar pekerjaan.
type Spec struct {
	RootID int64
	Op     string // OpUp atau OpDown
	SQL    string
	// Mode batch jika Size > 0. SQL memakai $1 (batas bawah, eksklusif) dan $2 (batas atas,
	// inklusif) pada Column, dan harus satu statement. Table berbentuk schema.table.
	Size   int
	Table  string
	Column string
}

type Progress struct {
	Batches, Rows int64
	Total         *int64 // jumlah row cakupan, jika diketahui
}

func (r *Runner) wait() time.Duration {
	if r.LockWait > 0 {
		return r.LockWait
	}
	return 5 * time.Second
}

func (r *Runner) stmtTimeout() time.Duration {
	if r.StatementTimeout > 0 {
		return r.StatementTimeout
	}
	return 10 * time.Minute
}

// IsDefinite: error dari server PostgreSQL (SQLSTATE), artinya transaksi pasti tidak ter-commit.
func IsDefinite(err error) bool { return errors.As(err, new(*pgconn.PgError)) }

// IsTransient: kegagalan sementara yang aman dicoba ulang.
func IsTransient(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "40001", "40P01", "55P03", "53300", "57P01", "57P03":
			return true
		}
		return false
	}
	return errors.Is(err, ErrBusy) || errors.Is(err, ErrRolledBack)
}

type session struct{ conn *pgxpool.Conn }

// hold mengambil session advisory lock pada koneksi khusus. Lock bertahan di antara commit batch
// karena terikat ke koneksi, bukan ke transaksi.
func (r *Runner) hold(ctx context.Context) (*session, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "lock.acquire")
	defer span.End()
	started := time.Now()
	conn, err := r.Pool.Acquire(ctx)
	if err != nil {
		span.RecordError(err)
		return nil, err
	}
	deadline := started.Add(r.wait())
	for {
		var ok bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(`+lockKey+`)`).Scan(&ok); err != nil {
			conn.Release()
			span.RecordError(err)
			return nil, err
		}
		if ok {
			waited := time.Since(started)
			if r.Metrics != nil {
				r.Metrics.LockWait(waited)
			}
			telemetry.Event(ctx, slog.LevelInfo, "lock.acquired", "ok", slog.Float64("wait_ms", float64(waited.Microseconds())/1000))
			return &session{conn}, nil
		}
		if time.Now().After(deadline) {
			conn.Release()
			telemetry.Event(ctx, slog.LevelWarn, "lock.timeout", "busy")
			span.RecordError(ErrBusy)
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (s *session) release() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ok bool
	if err := s.conn.QueryRow(ctx, `SELECT pg_advisory_unlock(`+lockKey+`)`).Scan(&ok); err != nil || !ok {
		// Menutup koneksi menjatuhkan session lock, jadi lock tidak mungkin tertinggal.
		s.conn.Conn().Close(ctx)
	}
	s.conn.Release()
}

func validate(sp Spec) error {
	if sp.RootID == 0 || (sp.Op != OpUp && sp.Op != OpDown) || sp.SQL == "" {
		return fmt.Errorf("%w: root, op, dan sql wajib", ErrInvalidSpec)
	}
	if sp.Size == 0 {
		return nil
	}
	if sp.Size < 0 || !ident.MatchString(sp.Column) {
		return fmt.Errorf("%w: ukuran batch atau kolom cursor", ErrInvalidSpec)
	}
	_, err := table(sp.Table)
	return err
}

func table(name string) (string, error) {
	parts := strings.Split(name, ".")
	if len(parts) != 2 || !ident.MatchString(parts[0]) || !ident.MatchString(parts[1]) {
		return "", fmt.Errorf("%w: cakupan batch harus berbentuk schema.table", ErrInvalidSpec)
	}
	return pgx.Identifier(parts).Sanitize(), nil
}

type queryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func ensureControl(ctx context.Context, q queryer) error {
	var runs, cps bool
	if err := q.QueryRow(ctx, `SELECT to_regclass('_platform.migration_runs') IS NOT NULL,
		to_regclass('_platform.migration_checkpoints') IS NOT NULL`).Scan(&runs, &cps); err != nil {
		return err
	}
	if runs && cps {
		return nil
	}
	_, err := q.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS _platform;
		CREATE TABLE IF NOT EXISTS _platform.migration_runs (
			root_job    bigint      NOT NULL,
			op          text        NOT NULL,
			scope_max   bigint,
			total_rows  bigint,
			started_at  timestamptz NOT NULL DEFAULT now(),
			finished_at timestamptz,
			PRIMARY KEY (root_job, op));
		CREATE TABLE IF NOT EXISTS _platform.migration_checkpoints (
			root_job      bigint      NOT NULL,
			op            text        NOT NULL,
			batch_no      int         NOT NULL,
			cursor_from   bigint      NOT NULL,
			cursor_to     bigint      NOT NULL,
			rows_affected bigint      NOT NULL,
			committed_at  timestamptz NOT NULL DEFAULT now(),
			PRIMARY KEY (root_job, op, batch_no))`)
	return err
}

type run struct {
	exists, finished bool
	scopeMax, total  *int64
}

func loadRun(ctx context.Context, q queryer, sp Spec) (run, error) {
	var r run
	var fin *time.Time
	err := q.QueryRow(ctx, `SELECT scope_max, total_rows, finished_at FROM _platform.migration_runs
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op).Scan(&r.scopeMax, &r.total, &fin)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	r.exists, r.finished = err == nil, fin != nil
	return r, err
}

func totals(ctx context.Context, q queryer, sp Spec, r run) (Progress, error) {
	p := Progress{Total: r.total}
	err := q.QueryRow(ctx, `SELECT count(*), coalesce(sum(rows_affected), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op).Scan(&p.Batches, &p.Rows)
	if err == nil && sp.Size == 0 && r.finished && r.total != nil {
		p.Batches, p.Rows = 1, *r.total
	}
	return p, err
}

// Evidence membaca bukti di target. Dipanggil setelah mengambil lock, sehingga koneksi lama yang
// putus sudah pasti selesai dan tidak bisa meng-commit belakangan.
func (r *Runner) Evidence(ctx context.Context, sp Spec) (Progress, bool, error) {
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, false, err
	}
	defer s.release()
	var ok bool
	if err := s.conn.QueryRow(ctx, `SELECT to_regclass('_platform.migration_runs') IS NOT NULL`).Scan(&ok); err != nil || !ok {
		return Progress{}, false, err
	}
	rn, err := loadRun(ctx, s.conn, sp)
	if err != nil {
		return Progress{}, false, err
	}
	p, err := totals(ctx, s.conn, sp, rn)
	return p, rn.finished, err
}

// Run menjalankan Spec sampai selesai atau gagal. Aman dipanggil ulang (retry, resume, duplicate
// delivery): pekerjaan yang sudah committed tidak diulang karena bukti di target diperiksa lebih dulu.
func (r *Runner) Run(ctx context.Context, sp Spec, report func(Progress)) (Progress, error) {
	if err := validate(sp); err != nil {
		return Progress{}, err
	}
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, err
	}
	defer s.release()
	if err := ensureControl(ctx, s.conn); err != nil {
		return Progress{}, err
	}
	rn, err := loadRun(ctx, s.conn, sp)
	if err != nil {
		return Progress{}, err
	}
	if rn.finished {
		return totals(ctx, s.conn, sp, rn)
	}
	if sp.Size == 0 {
		return r.runNormal(ctx, s, sp)
	}
	return r.runBatches(ctx, s, sp, rn, report)
}

func (r *Runner) begin(ctx context.Context, s *session) (pgx.Tx, error) {
	tx, err := s.conn.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL statement_timeout = %d; SET LOCAL lock_timeout = %d`,
		r.stmtTimeout().Milliseconds(), r.wait().Milliseconds())); err != nil {
		tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return tx, nil
}

// commit mengembalikan nil jika perubahan pasti committed, termasuk ketika jawaban commit hilang
// tetapi bukti di target menunjukkan commit terjadi.
func (r *Runner) commit(ctx context.Context, tx pgx.Tx, sp Spec, batchNo int) error {
	if h := r.Hooks.BeforeCommit; h != nil {
		if err := h(sp.Op, batchNo); err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if IsDefinite(err) {
			return err
		}
		if err := r.reconcile(ctx, sp, batchNo, err); err != nil {
			return err
		}
	}
	if h := r.Hooks.AfterCommit; h != nil {
		return h(sp.Op, batchNo)
	}
	return nil
}

// reconcile: commit gagal tanpa jawaban server. Baca bukti di target lewat sesi baru (menunggu
// koneksi lama benar-benar mati). Ada bukti = sudah committed (nil); tidak ada = pasti tidak
// committed (ErrRolledBack); tidak terbaca = ErrCommitUnknown.
func (r *Runner) reconcile(ctx context.Context, sp Spec, batchNo int, cause error) error {
	ectx, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.reconcileWait())
	defer cancel()
	s, err := r.hold(ectx)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUnknown, cause)
	}
	defer s.release()
	var committed bool
	if sp.Size == 0 {
		rn, err := loadRun(ectx, s.conn, sp)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrCommitUnknown, cause)
		}
		committed = rn.finished
	} else if err := s.conn.QueryRow(ectx, `SELECT EXISTS (SELECT 1 FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2 AND batch_no = $3)`, sp.RootID, sp.Op, batchNo).Scan(&committed); err != nil {
		return fmt.Errorf("%w: %v", ErrCommitUnknown, cause)
	}
	if committed {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrRolledBack, cause)
}

func (r *Runner) runNormal(ctx context.Context, s *session, sp Spec) (Progress, error) {
	tx, err := r.begin(ctx, s)
	if err != nil {
		return Progress{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	tag, err := tx.Exec(ctx, sp.SQL)
	if err != nil {
		return Progress{}, err
	}
	rows := tag.RowsAffected()
	if _, err := tx.Exec(ctx, `INSERT INTO _platform.migration_runs (root_job, op, total_rows, finished_at)
		VALUES ($1, $2, $3, now())`, sp.RootID, sp.Op, rows); err != nil {
		return Progress{}, err
	}
	if err := r.commit(ctx, tx, sp, 0); err != nil {
		return Progress{}, err
	}
	return Progress{Batches: 1, Rows: rows, Total: &rows}, nil
}

func (r *Runner) runBatches(ctx context.Context, s *session, sp Spec, rn run, report func(Progress)) (Progress, error) {
	tbl, _ := table(sp.Table)
	col := pgx.Identifier{sp.Column}.Sanitize()
	if !rn.exists {
		// Cakupan ditetapkan sekali sejak awal dan disimpan di target; baris baru setelah ini tidak ikut.
		var maxID, count *int64
		if err := s.conn.QueryRow(ctx, fmt.Sprintf(`SELECT max(%s), count(*) FROM %s`, col, tbl)).Scan(&maxID, &count); err != nil {
			return Progress{}, err
		}
		if _, err := s.conn.Exec(ctx, `INSERT INTO _platform.migration_runs (root_job, op, scope_max, total_rows, finished_at)
			VALUES ($1, $2, $3, $4, CASE WHEN $3::bigint IS NULL THEN now() END) ON CONFLICT (root_job, op) DO NOTHING`,
			sp.RootID, sp.Op, maxID, count); err != nil {
			return Progress{}, err
		}
		var err error
		if rn, err = loadRun(ctx, s.conn, sp); err != nil {
			return Progress{}, err
		}
		if rn.finished {
			return totals(ctx, s.conn, sp, rn)
		}
	}
	prog, err := totals(ctx, s.conn, sp, rn)
	if err != nil {
		return prog, err
	}
	var last int64
	var batchNo int
	if err := s.conn.QueryRow(ctx, `SELECT coalesce(max(cursor_to), $3), coalesce(max(batch_no), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = $2`, sp.RootID, sp.Op, int64(math.MinInt64)).Scan(&last, &batchNo); err != nil {
		return prog, err
	}
	if rn.scopeMax == nil {
		// run tanpa cakupan (bukan dibuat oleh mode batch untuk op ini): jangan menebak rentangnya.
		return prog, fmt.Errorf("%w: run %s tidak punya cakupan batch", ErrInvalidSpec, sp.Op)
	}
	next := fmt.Sprintf(`SELECT max(c) FROM (SELECT %s AS c FROM %s WHERE %s > $1 AND %s <= $2 ORDER BY %s LIMIT $3) s`, col, tbl, col, col, col)
	for {
		if err := ctx.Err(); err != nil {
			return prog, err
		}
		var upper *int64
		if err := s.conn.QueryRow(ctx, next, last, *rn.scopeMax, sp.Size).Scan(&upper); err != nil {
			return prog, err
		}
		if upper == nil {
			break
		}
		batchNo++
		bctx, span := telemetry.Tracer().Start(telemetry.WithBatch(ctx, batchNo), "batch.run")
		tx, err := r.begin(bctx, s)
		if err != nil {
			span.End()
			return prog, err
		}
		tag, err := tx.Exec(bctx, sp.SQL, last, *upper)
		if err == nil {
			_, err = tx.Exec(bctx, `INSERT INTO _platform.migration_checkpoints (root_job, op, batch_no, cursor_from, cursor_to, rows_affected)
				VALUES ($1, $2, $3, $4, $5, $6)`, sp.RootID, sp.Op, batchNo, last, *upper, tag.RowsAffected())
		}
		if err != nil {
			tx.Rollback(context.WithoutCancel(bctx))
			span.RecordError(err)
			span.End()
			return prog, err
		}
		if err := r.commit(bctx, tx, sp, batchNo); err != nil {
			span.RecordError(err)
			span.End()
			return prog, err
		}
		span.End()
		prog.Batches++
		prog.Rows += tag.RowsAffected()
		last = *upper
		if r.Metrics != nil {
			r.Metrics.BatchCommitted(tag.RowsAffected())
		}
		telemetry.Event(bctx, slog.LevelInfo, "batch.committed", "ok", slog.Int64("rows", tag.RowsAffected()),
			slog.Int64("committed_batches", prog.Batches), slog.Int64("committed_rows", prog.Rows))
		if report != nil {
			report(prog)
		}
	}
	if _, err := s.conn.Exec(ctx, `UPDATE _platform.migration_runs SET finished_at = now()
		WHERE root_job = $1 AND op = $2 AND finished_at IS NULL`, sp.RootID, sp.Op); err != nil {
		return prog, err
	}
	return prog, nil
}

type span struct {
	no       int
	from, to int64
}

// Compensate membatalkan batch up yang sudah committed memakai SQL down, dalam urutan batch terbalik.
// Setiap batch down adalah transaksi sendiri dengan checkpoint-nya sendiri, jadi kegagalan di tengah
// jalan meninggalkan keadaan partially_reverted yang bisa dilanjutkan dengan memanggil Compensate lagi.
func (r *Runner) Compensate(ctx context.Context, sp Spec, report func(Progress)) (Progress, error) {
	sp.Op, sp.Size = OpDown, 1
	if sp.Table == "" {
		sp.Table = "x.x"
	}
	if sp.Column == "" {
		sp.Column = "x"
	}
	if err := validate(sp); err != nil {
		return Progress{}, err
	}
	s, err := r.hold(ctx)
	if err != nil {
		return Progress{}, err
	}
	defer s.release()
	if err := ensureControl(ctx, s.conn); err != nil {
		return Progress{}, err
	}
	upRun, err := loadRun(ctx, s.conn, Spec{RootID: sp.RootID, Op: OpUp})
	if err != nil {
		return Progress{}, err
	}
	if !upRun.exists {
		return Progress{}, fmt.Errorf("%w: tidak ada bukti eksekusi up untuk dibatalkan", ErrUnsafe)
	}
	rows, err := s.conn.Query(ctx, `SELECT u.batch_no, u.cursor_from, u.cursor_to FROM _platform.migration_checkpoints u
		WHERE u.root_job = $1 AND u.op = 'up' AND NOT EXISTS (SELECT 1 FROM _platform.migration_checkpoints d
			WHERE d.root_job = u.root_job AND d.op = 'down' AND d.batch_no = u.batch_no)
		ORDER BY u.batch_no DESC`, sp.RootID)
	if err != nil {
		return Progress{}, err
	}
	pending, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (span, error) {
		var x span
		return x, r.Scan(&x.no, &x.from, &x.to)
	})
	if err != nil {
		return Progress{}, err
	}
	var upBatches int64
	if err := s.conn.QueryRow(ctx, `SELECT count(*) FROM _platform.migration_checkpoints WHERE root_job = $1 AND op = 'up'`,
		sp.RootID).Scan(&upBatches); err != nil {
		return Progress{}, err
	}
	if upBatches == 0 {
		// Tanpa checkpoint tidak ada rentang row yang bisa dikembalikan (mis. pekerjaan mode biasa);
		// menandai sukses di sini berarti melaporkan pemulihan yang tidak pernah terjadi.
		return Progress{}, fmt.Errorf("%w: eksekusi up tidak punya checkpoint batch", ErrUnsafe)
	}
	if _, err := s.conn.Exec(ctx, `INSERT INTO _platform.migration_runs (root_job, op, total_rows) VALUES ($1, 'down', $2)
		ON CONFLICT DO NOTHING`, sp.RootID, upBatches); err != nil {
		return Progress{}, err
	}
	prog := Progress{Total: &upBatches}
	if err := s.conn.QueryRow(ctx, `SELECT count(*), coalesce(sum(rows_affected), 0) FROM _platform.migration_checkpoints
		WHERE root_job = $1 AND op = 'down'`, sp.RootID).Scan(&prog.Batches, &prog.Rows); err != nil {
		return prog, err
	}
	for _, b := range pending {
		if err := ctx.Err(); err != nil {
			return prog, err
		}
		tx, err := r.begin(ctx, s)
		if err != nil {
			return prog, err
		}
		tag, err := tx.Exec(ctx, sp.SQL, b.from, b.to)
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO _platform.migration_checkpoints (root_job, op, batch_no, cursor_from, cursor_to, rows_affected)
				VALUES ($1, 'down', $2, $3, $4, $5)`, sp.RootID, b.no, b.from, b.to, tag.RowsAffected())
		}
		if err != nil {
			tx.Rollback(context.WithoutCancel(ctx))
			return prog, err
		}
		if err := r.commit(ctx, tx, sp, b.no); err != nil {
			return prog, err
		}
		prog.Batches++
		prog.Rows += tag.RowsAffected()
		if report != nil {
			report(prog)
		}
	}
	if _, err := s.conn.Exec(ctx, `UPDATE _platform.migration_runs SET finished_at = now()
		WHERE root_job = $1 AND op = 'down' AND finished_at IS NULL`, sp.RootID); err != nil {
		return prog, err
	}
	return prog, nil
}
