# Runggu Data

Runggu Data adalah platform pengelolaan database untuk Tugas Besar IF3110 Pengembangan Aplikasi Web. Aplikasi ini mengatur akses ke database target, menyediakan penjelajahan schema dan data, serta membantu pengguna meninjau perubahan schema. PostgreSQL menyimpan metadata platform, termasuk pengguna, session, group, grant, dan koneksi target.

## Implementasi

| Fitur | Cara kerja |
|---|---|
| Antarmuka web | React, TypeScript, dan Vite membangun SPA responsif. Server Go menyajikan aplikasi dan API JSON dari alamat yang sama. |
| Autentikasi | Login dengan password yang di-hash Argon2id, cookie session `HttpOnly`, rotasi identifier, CSRF, dan middleware untuk autentikasi serta izin admin. |
| Administrasi | Platform Admin mengelola pengguna, status akun, password, group, keanggotaan, grant, dan koneksi. Password koneksi disimpan dengan AES-256-GCM. |
| Database viewer | Pengguna membuka database PostgreSQL atau MariaDB sesuai grant dan melihat schema, struktur tabel, constraint, index, serta data dengan pagination 50 baris. |
| Schema diff | Membandingkan schema aktif dengan referensi pada DBMS yang sama, menampilkan perbedaan, lalu menghasilkan SQL kandidat untuk ditinjau. Layanan workflow menyimpan kandidat sebagai draft. |
| Execution plan | `EXPLAIN` menampilkan estimasi query PostgreSQL. `EXPLAIN ANALYZE` mengukur SELECT yang lolos validasi keamanan dalam transaksi read-only. |
| Migration M2 | SPA menyediakan editor up/down, alasan irreversible, review, history, progress job, dan pengajuan rollback, forward, atau pemulihan. Adapter API memanggil layanan workflow dan worker melalui kontrak Go. |
| Observability dan keamanan | OpenTelemetry mengirim log dan trace. Prometheus mengambil metrik HTTP dan pool koneksi. Grafana memuat dashboard secara otomatis. CodeQL memindai Go dan JavaScript/TypeScript. |

## Menjalankan Aplikasi

Lingkungan development menggunakan Go 1.26.11 atau lebih baru, Node.js 24+, npm, dan Docker Compose v2. Direktori kerja perintah berikut adalah root repository.

```powershell
go run ./cmd/envinit
```

`envinit` membuat `.env` dan key acak dari [.env.example](.env.example) saat file belum ada. Konfigurasi akun pertama memakai `BOOTSTRAP_ADMIN_USERNAME` dan `BOOTSTRAP_ADMIN_PASSWORD` di `.env` sebelum menjalankan server. Nilai contoh development adalah `admin` dan `12345678`. Database platform yang sudah memiliki admin menggunakan akun yang tersimpan.

### Dengan Make

```bash
make run
```

`make run` menyiapkan `.env`, menunggu database Compose siap, membangun SPA, lalu menjalankan server Go.

### Tanpa Make

```powershell
docker compose up -d --wait platform-db target-db mariadb-target
npm.cmd --prefix frontend ci
npm.cmd --prefix frontend run build
go run ./cmd/server
```

Pada Bash, perintah memakai `npm` sebagai pengganti `npm.cmd`. Build Vite menghasilkan aset di `web/static/spa/`. Perubahan sumber frontend memerlukan build SPA dan restart server karena binary Go menyertakan aset tersebut.

Server menjalankan migrasi schema platform saat startup. [Halaman login](http://localhost:8080/login) menerima akun platform. [Healthcheck](http://localhost:8080/healthz) memeriksa koneksi database platform.

Proses berikut berjalan pada terminal kedua dengan konfigurasi database platform dan key enkripsi yang sama. Worker diperlukan agar job dalam queue dieksekusi.

```powershell
go run ./cmd/worker
```

Perintah Make yang setara adalah `make worker`.

### Database lokal

| Service | Port host | Database | User | Password lokal |
|---|---|---|---|---|
| `platform-db` | 5433 | `platform` | `platform` | `platform` |
| `target-db` | 5434 | `target` | `target` | `target` |
| `test-db` | 5435 | `test` | `test` | `test` |
| `mariadb-target` | 3307 | `target` | `target` | `target` |
| `mariadb-test` (profile `test`) | 3308 | `test` | `root` | `test` |

Tabel mengikuti konfigurasi development di [docker-compose.yml](docker-compose.yml). Target PostgreSQL memuat contoh penerbangan pada schema `flight_ops` dan `maintenance`. Target MariaDB memuat tabel `airports` dan `flights` pada database `target`. Kedua database pengujian menggunakan penyimpanan sementara (`tmpfs`).

`Ctrl+C` menghentikan proses server atau worker pada terminalnya. `docker compose stop` menghentikan container development.

## Konfigurasi Environment

| Variabel | Default/contoh | Kegunaan |
|---|---|---|
| `PLATFORM_DATABASE_URL` | `postgres://platform:platform@localhost:5433/platform?sslmode=disable` | Koneksi database platform, wajib diisi |
| `HTTP_ADDR` | `:8080` | Alamat server HTTP |
| `APP_ENV` | `development` | `development`, `test`, atau `production` |
| `APP_ORIGIN` | `http://localhost:8080` pada `.env.example` | Origin publik untuk pemeriksaan CSRF, tanpa path atau trailing slash. Production memakai HTTPS. |
| `SESSION_KEY` | Base64 dari 32 byte acak | Key CSRF pre-session dan pemulihan token hasil rotasi. Wajib pada production. |
| `ENCRYPTION_KEY` | Base64 dari 32 byte acak | Key enkripsi password koneksi target. Wajib untuk mengelola koneksi dan pada production. |
| `BOOTSTRAP_ADMIN_USERNAME` | Kosong | Username admin pertama |
| `BOOTSTRAP_ADMIN_PASSWORD` | Kosong | Password admin pertama |
| `SESSION_IDLE_TIMEOUT` | `30m` | Batas session tanpa aktivitas |
| `SESSION_ABSOLUTE_TTL` | `8h` | Umur maksimum session sejak login |
| `SESSION_ROTATION_INTERVAL` | `15m` | Interval rotasi identifier |
| `SESSION_GRACE_PERIOD` | `30s` | Masa berlaku identifier sebelumnya setelah rotasi |
| `TEST_DATABASE_URL` | `postgres://test:test@localhost:5435/test?sslmode=disable` | Koneksi instance PostgreSQL khusus pengujian |
| `TEST_MARIADB_DSN` | `root:test@tcp(127.0.0.1:3308)/` | Koneksi instance MariaDB khusus pengujian |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `http://127.0.0.1:4318` | Alamat Collector untuk mengirim log dan trace |
| `GRAFANA_ADMIN_PASSWORD` | `local-observability` | Password akun Grafana development |

Durasi menggunakan format Go, misalnya `30m` atau `8h`. Konfigurasi harus memenuhi `0 < grace < rotation < absolute` dan `0 < idle <= absolute`.

## Alur Penggunaan

1. Platform Admin membuat pengguna, group, leader group, koneksi, dan grant. Koneksi PostgreSQL development: host `localhost`, port `5434`, database/user/password `target`, SSL `disable`.
2. Anggota dengan grant View melihat schema, data, history, dan job. Grant Edit dapat membuat draft dan pengajuan.
3. Draft berisi SQL up serta SQL down, atau ditandai irreversible beserta alasannya. SQL yang pernah dieksekusi tidak bisa diubah.
4. Pengajuan (apply, rollback, atau forward) dikirim untuk review dan harus disetujui leader setiap group yang memiliki akses ke database, baik View maupun Edit.
5. Pengajuan yang disetujui dimasukkan ke queue. API mengembalikan job ID, lalu progres dipantau di halaman Job.
6. Worker memeriksa ulang izin dan approval, mengambil lock database, menjalankan SQL, lalu mencatat setiap attempt (berhasil maupun gagal) di migration history.

**Mode batch (PostgreSQL).** Cursor berupa kolom integer `NOT NULL` dan unik. SQL memakai batas `cursor > $1 AND cursor <= $2` dan hanya boleh berupa `UPDATE`, karena hanya baris UPDATE yang tercatat sehingga dapat dikompensasi. Cakupan ditetapkan saat mulai. Setiap batch adalah transaksi sendiri dengan checkpoint di database target. Batch yang gagal hanya membatalkan transaksinya sendiri, sehingga job berstatus `partial`. Job partial dapat dilanjutkan (resume) dari checkpoint atau dikompensasi dengan SQL down dalam urutan batch terbalik. Kompensasi yang gagal di tengah berstatus `partially_reverted` dan dapat dilanjutkan. Selama ada job yang `partial`, `partially_reverted`, atau `commit_unknown`, migration lain pada database tersebut ditahan.

### Target MariaDB (bonus DBMS tambahan)

Koneksi MariaDB development: host `localhost`, port `3307`, database/user/password `target`, SSL `disable`. Explorer, schema diff, workflow, queue, history, rollback, dan forward mendukung MariaDB dalam mode normal. Satu migration berisi tepat satu DDL (karena DDL MariaDB melakukan implicit commit) atau beberapa DML InnoDB dalam satu transaksi. Mode batch hanya tersedia untuk PostgreSQL.

### Query execution plan analysis (bonus)

Editor migration PostgreSQL menyediakan **Analisis plan SQL up**. `EXPLAIN` menampilkan estimasi biaya dan scan. `EXPLAIN ANALYZE` hanya dijalankan untuk SELECT yang lolos pemeriksaan keamanan, di dalam transaksi read-only.

### Schema diff dan migration generation (bonus)

Schema diff membandingkan schema aktif dengan schema referensi pada DBMS yang sama, menampilkan perbedaan beserta risikonya, lalu menghasilkan SQL kandidat yang dapat disimpan sebagai draft. Penyimpanan draft tidak mengeksekusi SQL pada database target.

## Aturan Akses dan Session

### Hak akses database

| Kemampuan | Tanpa grant | View Only (`view`) | View + Edit (`edit`) |
|---|---|---|---|
| Melihat schema, struktur tabel, dan data | Ditolak | Diizinkan | Diizinkan |
| Melihat definisi migration dan history yang tersedia | Ditolak | Diizinkan | Diizinkan |
| Membuat dan mengubah draft migration | Ditolak | Ditolak | Diizinkan saat layanan workflow tersedia |
| Mengajukan dan memasukkan migration yang disetujui ke queue | Ditolak | Ditolak | Diizinkan saat layanan workflow tersedia |

Akses berasal dari grant group. Jika pengguna tergabung dalam beberapa group, server mengambil level tertinggi dari grant yang berlaku untuk database tersebut. Platform Admin juga memerlukan grant group untuk mengakses database target. Backend memeriksa grant pada setiap aksi database.

### Autentikasi dan CSRF

- Password menggunakan Argon2id dengan salt acak. Username tidak membedakan huruf besar dan kecil.
- Saat login berhasil, server mengirim identifier session acak 256 bit melalui cookie dan menyimpan hash SHA-256 identifier di database.
- Session berakhir setelah idle 30 menit atau mencapai umur 8 jam. Rotasi berlangsung tiap 15 menit dan ketika privilege ditingkatkan, dengan grace period 30 detik. Rotasi tidak memperpanjang absolute expiration.
- Compare-and-swap memastikan request bersamaan menggunakan hasil rotasi yang sama. CSRF token session tetap berlaku saat identifier dirotasi.
- Cookie memakai `HttpOnly`, `SameSite=Lax`, serta `Secure` pada HTTPS/production. Production menggunakan nama cookie `__Host-session`.
- Logout, penonaktifan akun, dan reset password mencabut session di backend. Policy database dibaca ulang sehingga perubahan grant berlaku pada request berikutnya.
- Request API yang mengubah state menggunakan header `X-CSRF-Token`. Login menggunakan signed double-submit cookie pre-session dengan masa berlaku 10 menit. Setelah login, SPA menggunakan token session. Server juga memeriksa origin dan menolak token dari query URL.
- Login dibatasi 10 percobaan per IP per menit per proses, dengan maksimal 4 verifikasi password bersamaan.

## Integrasi Migration M2

Server menyediakan halaman dan endpoint M2 melalui interface `Workflow`, `Leaders`, dan `Jobs`. [Entry point server](cmd/server/main.go) memasang implementasi dari package workflow dan jobs pada startup. [Kontrak Go](internal/contracts/platform.go) mendefinisikan DTO dan interface yang digunakan adapter API.

| Layanan | Tanggung jawab |
|---|---|
| `Workflow` | Menyimpan draft dan revisi, mengelola pengajuan serta approval, memasukkan job secara persisten, dan mencatat setiap attempt pada history. Approval terkait dengan revisi dan cakupan yang ditinjau. |
| `Leaders` | Menetapkan leader dari anggota group. Workflow memeriksa approval leader dari group yang memiliki akses ke database target, termasuk View Only. |
| `Jobs` | Menyediakan status dan progress job. Resume dan kompensasi menghasilkan pengajuan pemulihan untuk ditinjau. |

Adapter memeriksa grant sebelum memanggil layanan. Enqueue memerlukan header `Idempotency-Key` dan mengembalikan `202` beserta job ID setelah job tersimpan. Worker menjalankan SQL secara asynchronous serta memeriksa ulang izin, revisi, approval, dan checkpoint. Daftar route, termasuk `/api/auth`, `/api/admin`, `/api/db/{id}/migrations`, `/api/db/{id}/requests`, dan `/api/db/{id}/jobs`, ada di [router API](internal/httpx/api.go).

Worker memakai `WORKER_ID` (default hostname-PID), `WORKER_LEASE=30s`, `WORKER_POLL=1s`, `WORKER_LOCK_WAIT=30s`, `WORKER_SQL_TIMEOUT=30m`, `WORKER_METRICS_ADDR=:9102`, dan `WORKER_CONCURRENCY=4`. `WORKER_CONCURRENCY` adalah jumlah job yang dapat dijalankan satu proses worker secara bersamaan, sehingga job pada database berbeda berjalan paralel tanpa proses worker tambahan. Job pada database yang sama tetap dijalankan satu per satu. Setiap worker memakai alamat metrics yang tersedia pada hostnya.

### Pemulihan lock ketika worker gagal

Perlindungan memakai dua lapis. Keduanya pulih tanpa campur tangan manual ketika worker mati.

**1. Lease job di database platform.** `jobs.Claim` ([`internal/jobs/jobs.go`](internal/jobs/jobs.go)) mengubah job menjadi `running` dengan `worker_id` dan `lease_until = now() + WORKER_LEASE`. Worker memperbarui lease lewat heartbeat setiap sepertiga `WORKER_LEASE`. Semua perubahan status memakai fencing `WHERE worker_id = $worker AND lease_until > now()`. Akibatnya, worker lama yang kehilangan lease tidak bisa lagi menulis hasil (`ErrLeaseLost`), lalu menghentikan pekerjaannya sendiri. Job `running` dengan lease kedaluwarsa dapat diklaim ulang oleh worker lain. Job pemulihan (`attempt_open` atau `commit_unknown`) diklaim lebih dulu dan menahan job lain di target yang sama.

**2. Lock eksekusi di database target.** Sebelum SQL berjalan, worker mengambil `pg_try_advisory_lock(hashtext('if3110.migration'))` pada koneksi khusus ([`internal/batch/batch.go`](internal/batch/batch.go)). Untuk MariaDB, worker memakai `GET_LOCK` per database. Lock ini *session-level*, sehingga bertahan di antara commit batch dan baru dilepas setelah seluruh migration selesai. Advisory lock PostgreSQL berlaku per database, jadi database berbeda tetap dapat diproses paralel.

**Ketika worker gagal:**

| Kejadian | Lock target | Lease job | Pemulihan |
|---|---|---|---|
| Proses worker mati atau crash | Koneksi tertutup, dan database otomatis melepas session lock | Heartbeat berhenti, lease kedaluwarsa setelah `WORKER_LEASE` | Worker lain mengklaim job, mengambil lock, lalu membaca bukti di target sebelum melanjutkan |
| Worker hang atau jaringan ke platform putus | Lock tetap dipegang selama koneksi target masih hidup | Heartbeat gagal, worker membatalkan context, melepas lock, dan menutup koneksi | Worker pengganti menunggu lock maksimal `WORKER_LOCK_WAIT`. Jika masih sibuk (`ErrBusy`), job dijadwalkan ulang dengan backoff dan batas percobaan |
| Unlock gagal saat selesai | Koneksi ditutup paksa supaya session lock pasti lepas (`session.release`) | Tidak berubah | Tidak ada lock yang tertinggal |

Selama worker lama masih memegang lock target, worker pengganti tidak bisa menjalankan SQL di database yang sama. Karena itu tidak terjadi double commit, walaupun lease sudah berpindah tangan. Worker pengganti tidak mengulang batch secara buta. Ia membaca checkpoint dan bukti ber-HMAC di skema `_platform` target, yang ditulis dalam transaksi yang sama dengan perubahan batch. Batch yang sudah committed dilewati. Jika bukti tidak dapat dibaca, job ditandai `commit_unknown` dan migration lain pada database itu ditahan sampai direkonsiliasi lewat resume atau kompensasi.

## Observability

```powershell
docker compose --profile observability up -d otel-collector loki tempo prometheus grafana
$env:OTEL_EXPORTER_OTLP_ENDPOINT = 'http://127.0.0.1:4318'
$env:OTEL_EXPORTER_OTLP_INSECURE = 'true'
go run ./cmd/server
```

Grafana menggunakan akun `admin` dan password dari `GRAFANA_ADMIN_PASSWORD`. Dashboard **IF3110 - Queue, Migration & Pool** dimuat bersama tiga datasource. Collector meneruskan log ke Loki dan trace ke Tempo. Prometheus mengambil metrik request HTTP dan pool database dari `/metrics`. Worker mengekspor metrik queue, batch, dan pool pada port 9102. Prometheus mengambil metrik API pada port 8080 dan worker pada port 9102. [Grafana development](http://127.0.0.1:3000), [konfigurasi Prometheus](deploy/observability/prometheus.yaml), dan [dashboard Grafana](deploy/observability/grafana/dashboards/platform.json) menyediakan akses serta konfigurasi observability.

### Contoh log dan span

Contoh berikut diambil dari server dan worker lokal tanpa Collector. Tanpa `OTEL_EXPORTER_OTLP_ENDPOINT`, span ditulis ke stdout memakai exporter console. Skenarionya satu migration `apply` yang berhasil (job 1) dan satu migration yang gagal karena unique violation (job 2). Setiap baris log adalah JSON dari `log/slog`. Baris dipotong pada atribut yang tidak relevan.

**API menerima enqueue** (server):

```json
{"time":"2026-11-27T07:37:56.5406331+07:00","level":"INFO","msg":"http.request","method":"POST","status":202,"duration_ms":14,"request_id":"71ca963c-985a-445e-9a93-04c134267b79","trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"683bcd9b14229b1f"}
```

**Worker mengklaim job, mengambil lock, menjalankan SQL, lalu commit** (worker, trace yang sama):

```json
{"time":"2026-11-27T07:37:57.4880042+07:00","level":"INFO","msg":"job.claimed","event":"job.claimed","status":"running","actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","correlation_id":"b1d3af18-8777-468a-9043-d67c849068a6","job_id":1,"migration_id":"8","request_id":"b1d3af18-8777-468a-9043-d67c849068a6","attempt":1,"op":"apply","kind":"run","retry":0,"trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"65345986fb776f04"}
{"time":"2026-11-27T07:37:57.5221513+07:00","level":"INFO","msg":"lock.acquired","event":"lock.acquired","status":"ok","actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","correlation_id":"b1d3af18-8777-468a-9043-d67c849068a6","job_id":1,"migration_id":"8","execution_id":"254b0cab-aa7b-4c35-93a2-b2c61039a50e","attempt":1,"op":"apply","wait_ms":4.344,"trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"606e3c1a117ce8d3"}
{"time":"2026-11-27T07:37:57.5722716+07:00","level":"INFO","msg":"sql.start","event":"sql.start","status":"started","query":"CREATE TABLE public.demo_log_1795739876 (id bigint PRIMARY KEY, note text)","params":[],"actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","job_id":1,"migration_id":"8","execution_id":"254b0cab-aa7b-4c35-93a2-b2c61039a50e","attempt":1,"op":"apply","trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"b7a55464cdae219c"}
{"time":"2026-11-27T07:37:57.5759091+07:00","level":"INFO","msg":"sql.result","event":"sql.result","status":"ok","duration_ms":3.637,"rows":0,"actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","job_id":1,"migration_id":"8","execution_id":"254b0cab-aa7b-4c35-93a2-b2c61039a50e","attempt":1,"op":"apply","trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"b7a55464cdae219c"}
{"time":"2026-11-27T07:37:57.5858895+07:00","level":"INFO","msg":"sql.commit.start","event":"sql.commit.start","status":"started","query":"commit","params":[],"actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","job_id":1,"migration_id":"8","execution_id":"254b0cab-aa7b-4c35-93a2-b2c61039a50e","attempt":1,"op":"apply","trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"53bc3203c90f6342"}
{"time":"2026-11-27T07:37:57.586638+07:00","level":"INFO","msg":"job.finished","event":"job.finished","status":"success","actor":"1","database":"b1143856-3cf2-4d03-8af8-022c58c8eb0e","job_id":1,"migration_id":"8","execution_id":"254b0cab-aa7b-4c35-93a2-b2c61039a50e","attempt":1,"op":"apply","hold":"none","trace_id":"6f08a1336c88c111531ed5c4d33d83f8","span_id":"65345986fb776f04"}
```

**Kegagalan SQL** (job 2). Error dicatat bersama SQLSTATE, dan literal pada query disamarkan menjadi `'?'`:

```json
{"time":"2026-11-27T07:38:43.7762627+07:00","level":"WARN","msg":"sql.result","event":"sql.result","status":"error","duration_ms":1.514,"error":"Database error (SQLSTATE 23505).","sqlstate":"23505","actor":"1","database":"8cfa5b52-1bb7-482e-9d02-98ac1d940f14","job_id":2,"migration_id":"9","execution_id":"1068f7f1-388a-4a57-a8c6-e08e5549efb0","attempt":1,"op":"apply","trace_id":"c999aa0ab1175adbab916bcf5ca3da0a","span_id":"3e7a70157f9fcc3d"}
{"time":"2026-11-27T07:38:43.7883442+07:00","level":"INFO","msg":"job.finished","event":"job.finished","status":"failed","actor":"1","database":"8cfa5b52-1bb7-482e-9d02-98ac1d940f14","job_id":2,"migration_id":"9","execution_id":"1068f7f1-388a-4a57-a8c6-e08e5549efb0","attempt":1,"op":"apply","hold":"none","trace_id":"c999aa0ab1175adbab916bcf5ca3da0a","span_id":"4247257f90698ba3"}
```

**Rantai span job 1** (trace `6f08a1336c88c111531ed5c4d33d83f8`). Trace context ikut tersimpan bersama job di queue, sehingga span worker menjadi anak dari span request API:

| Span | Proses | Span ID | Parent | Durasi |
|---|---|---|---|---|
| `http.request` (POST enqueue, 202) | server | `683bcd9b14229b1f` | (root) | 14 ms |
| `job.run` | worker | `65345986fb776f04` | `683bcd9b14229b1f` | 98 ms |
| `lock.acquire` | worker | `606e3c1a117ce8d3` | `65345986fb776f04` | 5 ms |
| `db.query` (CREATE TABLE) | worker | `b7a55464cdae219c` | `65345986fb776f04` | 3,6 ms |

**Span query yang gagal** (output exporter console, job 2, diringkas):

```json
{
  "Name": "db.query",
  "SpanContext": {"TraceID": "c999aa0ab1175adbab916bcf5ca3da0a", "SpanID": "3e7a70157f9fcc3d"},
  "Parent": {"TraceID": "c999aa0ab1175adbab916bcf5ca3da0a", "SpanID": "4247257f90698ba3"},
  "SpanKind": 3,
  "StartTime": "2026-11-27T07:38:43.7747481+07:00",
  "EndTime": "2026-11-27T07:38:43.7762627+07:00",
  "Attributes": [
    {"Key": "db.system", "Value": {"Type": "STRING", "Value": "postgresql"}},
    {"Key": "db.statement", "Value": {"Type": "STRING", "Value": "INSERT INTO public.demo_log_1795739876 (id, note) VALUES (1, '?'), (1, '?')"}}
  ],
  "Events": [{"Name": "exception", "Attributes": [
    {"Key": "exception.type", "Value": {"Type": "STRING", "Value": "*errors.errorString"}},
    {"Key": "exception.message", "Value": {"Type": "STRING", "Value": "Database error (SQLSTATE 23505)."}}
  ]}],
  "Status": {"Code": "Error", "Description": "Database error (SQLSTATE 23505)."},
  "Resource": [
    {"Key": "service.name", "Value": {"Type": "STRING", "Value": "if3110-worker"}},
    {"Key": "service.version", "Value": {"Type": "STRING", "Value": "2.0.0"}}
  ]
}
```

Credential koneksi, cookie, dan token session tidak muncul di log. Password koneksi target hanya disimpan terenkripsi (`password_enc`) dan tidak ikut dicatat.

## CodeQL

[Workflow CodeQL](.github/workflows/codeql.yml) memindai Go dan JavaScript/TypeScript dengan suite `security-extended`. GitHub Actions menyimpan hasil SARIF per bahasa sebagai artifact run terkait.

Cakupan scan: seluruh kode Go (`cmd/`, `internal/`) dan sumber SPA (`frontend/src`). Direktori `frontend/node_modules`, hasil build `web/static/spa`, dan `artifacts` dikecualikan melalui [konfigurasi CodeQL](.github/codeql/config.yml). Rule `go/sql-injection` tidak dimatikan, sehingga query katalog dan query internal tetap diperiksa.

### Temuan

Scan pada [PR #19](https://github.com/Lloyd565/IF3110-Pengembangan-Aplikasi-Web/pull/19) menghasilkan empat temuan. Semuanya berasal dari rule yang sama dan berada pada jalur eksekusi migration MariaDB.

| # | Rule | Severity | Lokasi | Potensi dampak | Keputusan |
|---|---|---|---|---|---|
| 1 | `go/sql-injection` (CWE-89) | Error / High | [`internal/migration/workflow_target.go:182`](internal/migration/workflow_target.go#L182): worker M2 mengeksekusi satu statement DDL migration | Pengguna dengan izin dapat menjalankan SQL apa pun dalam batas hak kredensial koneksi target | Risiko diterima (by design) |
| 2 | `go/sql-injection` (CWE-89) | Error / High | [`internal/migration/workflow_target.go:213`](internal/migration/workflow_target.go#L213): worker M2 mengeksekusi statement DML migration dalam satu transaksi InnoDB | Sama dengan temuan 1 | Risiko diterima (by design) |
| 3 | `go/sql-injection` (CWE-89) | Error / High | [`internal/migration/mariadb.go:429`](internal/migration/mariadb.go#L429): executor sinkron M1 (`runMaria`) mengeksekusi DDL | Sama dengan temuan 1, tanpa approval leader | Risiko diterima: jalur tidak aktif pada build produksi |
| 4 | `go/sql-injection` (CWE-89) | Error / High | [`internal/migration/mariadb.go:447`](internal/migration/mariadb.go#L447): executor sinkron M1 (`runMaria`) mengeksekusi DML | Sama dengan temuan 3 | Risiko diterima: jalur tidak aktif pada build produksi |

**Alasan.** Keempat sink adalah titik eksekusi isi migration, yaitu SQL up/down yang memang ditulis pengguna. Menjalankan SQL tersebut adalah fungsi utama platform. Parameterisasi tidak dapat diterapkan karena placeholder hanya menggantikan nilai, bukan statement DDL/DML utuh. Temuan ini benar secara aliran data, tetapi bukan injeksi ke query milik aplikasi.

Temuan 3 dan 4 berada pada executor sinkron M1. Executor ini hanya dipanggil oleh route `POST /db/{id}/migrations/{mid}/execute` pada mode template lama. Server produksi selalu berjalan dengan `SPAEnabled: true` ([`cmd/server/main.go`](cmd/server/main.go)). Pada mode itu route tersebut tidak terdaftar, dan semua permintaan non-GET di luar API dijawab `410 legacy_endpoint_disabled`. Akibatnya setiap eksekusi wajib melewati queue, approval, dan worker. Kode M1 dipertahankan untuk pengujian regresi M1.

**Mitigasi pada jalur worker M2 (temuan 1 dan 2):**

- Hanya pengguna dengan grant edit yang dapat membuat dan mengajukan migration.
- Setiap eksekusi, termasuk rollback dan forward, wajib disetujui leader dari setiap group yang memiliki akses ke database. Revisi SQL bersifat immutable dan terikat pada approval.
- Worker memeriksa ulang izin, approval, revisi, dan identitas server target tepat sebelum eksekusi (`revalidate`, `verifyMariaServer`).
- `mariaSQL` ([`internal/migration/mariadb.go`](internal/migration/mariadb.go)) membatasi DDL menjadi satu statement per migration tanpa campuran DML. Pembatasan lainnya adalah engine InnoDB saja, tanpa CTAS/LIKE, dan DROP/RENAME untuk satu objek.
- `mariaScope` menolak akses ke tabel kontrol internal dan fungsi `release_lock`/`release_all_locks`.
- Status tabel kontrol dibandingkan sebelum dan sesudah eksekusi. Perubahan dari SQL migration membatalkan transaksi DML (`ErrUnsafe`) atau menandai hasil DDL sebagai `commit_unknown`.

**Risiko sisa.** SQL berjalan dengan kredensial koneksi target. Belum ada pemisahan role database antara akun migration dan akun metadata platform. Admin sebaiknya mendaftarkan koneksi dengan akun yang haknya terbatas pada schema aplikasi.

Keempat alert di-dismiss pada GitHub Code Scanning dengan status *Won't fix* beserta alasan di atas. Hasil scan lain tetap aktif pada setiap push dan pull request.

## Pengujian

Suite integrasi menggunakan instance PostgreSQL dan MariaDB khusus pengujian. Konfigurasi berikut mencakup kedua DBMS.

```powershell
docker compose up -d --wait test-db mariadb-test
$env:TEST_DATABASE_URL = 'postgres://test:test@localhost:5435/test?sslmode=disable'
$env:TEST_MARIADB_DSN = 'root:test@tcp(127.0.0.1:3308)/'
go vet ./...
go test -race ./... -count=1
```

Pada Bash, perintah memakai `export TEST_DATABASE_URL='postgres://test:test@localhost:5435/test?sslmode=disable'` dan `export TEST_MARIADB_DSN='root:test@tcp(127.0.0.1:3308)/'`. Race detector memerlukan dukungan CGO dan compiler C pada platform yang digunakan.

Jika `TEST_DATABASE_URL` atau `TEST_MARIADB_DSN` kosong, test integrasi terkait akan dilewati. Helper membuat ulang database atau schema bernama `it_*` pada instance khusus test. Suite memerlukan hak `CREATE DATABASE` dan akses admin pada instance MariaDB untuk menyiapkan akun fixture.

Suite mencakup autentikasi, session paralel, CSRF, grant, administrasi, explorer, executor M1, schema diff, revisi, approval, queue, locking, batch, history, dan pemulihan job. Pengujian schema diff menjalankan kandidat DDL pada database sementara dan membandingkan hasilnya dengan referensi.

Dataset pengujian migration berisi 2.000.000 baris (`perf.accounts`). Satu UPDATE seluruh baris tanpa batching membutuhkan 9,217 s. Dataset yang sama dalam mode batch 10.000 baris selesai dalam 16,043 s (200 batch). [Pengujian performa](tests/performance/README.md) berisi cara membuat dataset, skrip k6, threshold, serta hasil dan analisis untuk setiap tingkat beban.
