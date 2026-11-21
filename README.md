# IF3110 - Database Management Platform

Tugas Besar IF3110 Pengembangan Aplikasi Web. Aplikasi ini mengatur akses ke database target, menyediakan penjelajahan schema dan data, serta membantu pengguna meninjau perubahan schema. PostgreSQL menyimpan metadata platform, termasuk pengguna, session, group, grant, dan koneksi target.

## Implementasi

| Fitur | Cara kerja |
|---|---|
| Antarmuka web | React, TypeScript, dan Vite membangun SPA responsif. Server Go menyajikan aplikasi dan API JSON dari alamat yang sama. |
| Autentikasi | Login dengan password yang di-hash Argon2id, cookie session `HttpOnly`, rotasi identifier, CSRF, dan middleware untuk autentikasi serta izin admin. |
| Administrasi | Platform Admin mengelola pengguna, status akun, password, group, keanggotaan, grant, dan koneksi. Password koneksi disimpan dengan AES-256-GCM. |
| Database viewer | Pengguna membuka database PostgreSQL atau MariaDB sesuai grant dan melihat schema, struktur tabel, constraint, index, serta data dengan pagination 50 baris. |
| Schema diff | Membandingkan schema aktif dengan referensi pada DBMS yang sama, menampilkan perbedaan, lalu menghasilkan SQL kandidat untuk ditinjau. Penyimpanan sebagai draft memerlukan layanan workflow. |
| Execution plan | Menjalankan `EXPLAIN` dan `EXPLAIN ANALYZE` untuk membantu pengguna Edit meninjau SQL pada PostgreSQL. |
| Migration M2 | SPA menyediakan editor up/down, alasan irreversible, review, history, progress job, dan pengajuan rollback, forward, atau pemulihan. Adapter API memanggil layanan workflow dan worker melalui kontrak Go. |
| Observability dan keamanan | OpenTelemetry mengirim log dan trace. Prometheus mengambil metrik HTTP dan pool koneksi. Grafana memuat dashboard secara otomatis. CodeQL memindai Go dan JavaScript/TypeScript. |

Login, administrasi, viewer, preview schema diff, dan execution plan dapat digunakan sekarang. Penyimpanan draft M2, approval, history eksekusi, dan job nyata memerlukan integrasi layanan workflow dan worker. Rincian kontraknya ada di [Integrasi Migration M2](#integrasi-migration-m2).

## Menjalankan Aplikasi

Siapkan Go 1.25.4 atau lebih baru, Node.js 24+, npm, dan Docker Compose v2. Jalankan perintah dari root repository.

```powershell
go run ./cmd/envinit
```

`envinit` membuat `.env` dan key acak dari [.env.example](.env.example) saat file belum ada. Untuk akun pertama, atur `BOOTSTRAP_ADMIN_USERNAME` dan `BOOTSTRAP_ADMIN_PASSWORD` di `.env` sebelum menjalankan server. Nilai contoh development adalah `admin` dan `12345678`. Jika database platform sudah memiliki admin, gunakan akun yang sudah ada.

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

Pada Bash, gunakan `npm` sebagai pengganti `npm.cmd`. Build Vite menghasilkan aset di `web/static/spa/`. Build ulang frontend dan jalankan ulang server setiap kali sumber frontend berubah karena binary Go menyertakan aset tersebut.

Server menjalankan migrasi schema platform saat startup. Setelah server menyala, masuk dengan akun platform. Buka [halaman login](http://localhost:8080/login) atau periksa koneksi database platform melalui [healthcheck](http://localhost:8080/healthz).

### Database lokal

| Service | Port host | Database | User | Password lokal |
|---|---|---|---|---|
| `platform-db` | 5433 | `platform` | `platform` | `platform` |
| `target-db` | 5434 | `target` | `target` | `target` |
| `test-db` | 5435 | `test` | `test` | `test` |
| `mariadb-target` | 3307 | `target` | `target` | `target` |
| `mariadb-test` (profile `test`) | 3308 | `test` | `root` | `test` |

Tabel mengikuti konfigurasi development di [docker-compose.yml](docker-compose.yml). Target PostgreSQL memuat contoh penerbangan pada schema `flight_ops` dan `maintenance`. Target MariaDB memuat tabel `airports` dan `flights` pada database `target`. Kedua database pengujian menggunakan penyimpanan sementara (`tmpfs`).

Tekan `Ctrl+C` untuk menghentikan server. Jalankan `docker compose stop` untuk menghentikan container development.

## Konfigurasi Environment

| Variabel | Default/contoh | Kegunaan |
|---|---|---|
| `PLATFORM_DATABASE_URL` | `postgres://platform:platform@localhost:5433/platform?sslmode=disable` | Koneksi database platform, wajib diisi |
| `HTTP_ADDR` | `:8080` | Alamat server HTTP |
| `APP_ENV` | `development` | `development`, `test`, atau `production` |
| `APP_ORIGIN` | `http://localhost:8080` pada `.env.example` | Origin publik untuk pemeriksaan CSRF, tanpa path atau trailing slash. Gunakan HTTPS pada production. |
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

Di development, `SESSION_KEY` kosong menghasilkan key sementara saat startup. Menetapkan key yang stabil menjaga validitas CSRF pre-session dan pemulihan cookie rotasi antar-restart. `ENCRYPTION_KEY` kosong masih memungkinkan login, tetapi koneksi target tidak dapat dibuat atau dibuka. Simpan key enkripsi yang sama agar password koneksi yang sudah tersimpan tetap bisa didekripsi.

Pada production, gunakan reverse proxy HTTPS, `APP_ENV=production`, origin HTTPS publik, serta key yang sama pada seluruh instance aplikasi. Dalam mode ini, isi konfigurasi melalui environment proses. Gunakan `.env.example` untuk membagikan contoh konfigurasi. Simpan kredensial development di `.env`, yang tercantum pada `.gitignore`.

## Alur Penggunaan

1. Login sebagai Platform Admin.
2. Buka **Pengguna** untuk membuat akun pengguna.
3. Buka **Group & anggota**, buat group, lalu tambahkan pengguna sebagai anggota.
4. Buka **Koneksi database**, pilih **PostgreSQL**, dan gunakan konfigurasi target lokal berikut.

   | Field | Nilai |
   |---|---|
   | Nama | Bebas, misalnya `Flight Operations` |
   | Host | `localhost` |
   | Port | `5434` |
   | Database | `target` |
   | Username | `target` |
   | Password | `target` |
   | SSL mode | `disable` |

5. Gunakan **Test connection**, lalu simpan koneksi. Target DB contoh diinisialisasi oleh [scripts/target-seed.sql](scripts/target-seed.sql) ketika volume Target DB pertama kali dibuat.
6. Buka **Hak akses**, pilih group dan koneksi, lalu berikan level `view` atau `edit`.
7. Login sebagai pengguna anggota group tersebut. Database yang diizinkan muncul pada dashboard.
8. Pilih **Buka workspace**, lalu gunakan tab **Schema**, **Migration**, **Review & approval**, **History**, **Job**, atau **Schema diff**.
9. Setelah layanan workflow terpasang, pengguna Edit dapat membuat draft dengan SQL up serta SQL down atau alasan irreversible. Contoh berikut menambahkan tabel pada database development.

   ```sql
   CREATE TABLE flight_ops.demo_notes (
       id bigserial PRIMARY KEY,
       note text NOT NULL
   );
   ```

10. Tinjau definisi dan kirim pengajuan ke leader group yang memiliki akses. Setelah disetujui, masukkan pengajuan ke queue dan pantau status eksekusi melalui job ID. Halaman job menampilkan attempt, jumlah baris dan batch yang sudah commit, serta error jika ada.

Saat layanan M2 belum terpasang, SPA menampilkan status ketersediaan dan API mengembalikan `503 module_unavailable` pada operasi yang bergantung pada layanan itu. Definisi migration M1 dapat dibaca sebagai data legacy.

Platform Admin tetap memerlukan keanggotaan group dan grant untuk membuka database. Tambahkan akun admin ke group bila admin juga ingin menggunakan explorer atau menjalankan migration.

### Target MariaDB (bonus DBMS tambahan)

Pada **Koneksi database**, pilih `MariaDB`, host `localhost`, port `3307`, database `target`, username/password `target`, dan SSL `disable` untuk target Compose lokal. Pada explorer MariaDB, database koneksi ditampilkan sebagai schema. Grant View/Edit dan pemeriksaan backend berlaku sama seperti PostgreSQL. Gunakan akun target yang haknya dibatasi pada database tersebut.

SSL MariaDB mendukung `disable`, `require` (enkripsi tanpa verifikasi sertifikat), dan `verify-full` (verifikasi sertifikat serta hostname). MariaDB sebelum 10.6.1 dan DBMS yang tidak didukung ditolak saat test/open connection. Koneksi PostgreSQL yang sudah ada tetap bertipe PostgreSQL setelah migrasi platform otomatis `0002_connection_dbms`.

Executor MariaDB yang tersedia pada fondasi aplikasi memakai aturan berikut.

- Satu migration schema berisi **tepat satu statement DDL** tabel, index, atau view, misalnya `ALTER TABLE flights ADD COLUMN gate VARCHAR(10);`.
- DDL MariaDB melakukan implicit commit. Validasi menolak beberapa DDL sekaligus dan campuran DDL-DML.
- Batch `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, dan `SELECT` dijalankan dalam transaksi. Database yang mengandung tabel non-InnoDB ditolak untuk batch DML.
- Kontrol transaksi, perintah administrasi, executable comments, `CREATE OR REPLACE TABLE`, `CREATE TABLE AS SELECT/LIKE`, DDL partisi, serta DROP/RENAME beberapa objek tidak didukung. Referensi ke database lain ditolak.
- Analisis execution plan tersedia pada koneksi PostgreSQL.
- Riwayat dan penomoran versi tetap dicatat pada Platform DB. Jika server/koneksi terputus saat eksekusi, periksa kondisi Target DB sebelum membuat migration perbaikan.

Perbedaan transaksi ini mengikuti [implicit commit MariaDB](https://mariadb.com/docs/server/reference/sql-statements/transactions/sql-statements-that-cause-an-implicit-commit) dan [atomic DDL MariaDB](https://mariadb.com/docs/server/reference/sql-statements/data-definition/atomic-ddl).

### Query execution plan analysis (bonus)

Pada editor migration PostgreSQL, pengguna dengan hak Edit dapat menekan **Analisis plan SQL up**. Jika tab **Migration** belum menampilkan tombol **Draft baru**, buka `/db/{id}/migrations/new` dengan ID dari URL workspace. `EXPLAIN` menampilkan estimasi biaya, jumlah baris, scan, index, join, dan filter. `EXPLAIN ANALYZE` juga menjalankan query untuk mengukur jumlah baris dan waktu aktual. Server membatalkan transaksi setelah analisis.

### Schema diff dan migration generation (bonus)

1. Buka tab **Schema diff** pada workspace database aktif.
2. Pilih koneksi referensi yang berisi kondisi schema yang diinginkan. Pilihan schema dimuat otomatis. Referensi dapat berupa schema lain pada koneksi yang sama atau koneksi lain dengan DBMS yang sama.
3. Pilih schema aktif dan schema referensi, lalu klik **Bandingkan dan buat SQL kandidat**. Kedua koneksi memerlukan hak View.
4. Tinjau perubahan dan SQL. Perbandingan mencakup tabel biasa, kolom bertipe builtin, default, nullability, primary/unique/foreign key, check, serta index BTREE biasa. Foreign key lokal dipetakan ke schema aktif dan SQL diurutkan sesuai dependensi.
5. Dengan hak Edit dan layanan workflow yang terpasang, beri nama lalu simpan draft. Perubahan yang menghapus tabel atau kolom atau mengubah tipe memerlukan konfirmasi pada form. Diff membaca perubahan nama sebagai penghapusan nama lama dan penambahan nama baru.
6. PostgreSQL mengirim satu kandidat gabungan ke editor. MariaDB membagi kandidat per statement sesuai urutan dependensi. Lengkapi SQL down atau alasan irreversible, lalu kirim pengajuan untuk review.
7. Setelah worker melaporkan eksekusi selesai, bandingkan kembali untuk memeriksa kondisi schema.

Server membuat preview SQL dari metadata katalog. Saat menyimpan draft, server menghitung ulang kandidat, memeriksa hak akses, dan mengembalikan `409` jika fingerprint schema berubah. Layanan `SaveGenerated` menyimpan rangkaian draft secara atomik pada database platform. Eksekusi DDL MariaDB mengikuti aturan implicit commit pada database target.

Objek khusus seperti view, partition, trigger, kolom generated, tipe kustom, partial index, dan foreign key lintas schema memerlukan penanganan manual. Nilai data, owner, grant, fungsi, dan sequence mandiri berada di luar cakupan perbandingan. Komentar kolom MariaDB dipertahankan saat kolom diubah.

Tinjau data dan SQL sebelum mengajukan perubahan tipe, `NOT NULL`, atau constraint baru. Batas satu perbandingan adalah 200 tabel, 2.000 kolom, dan 250 statement.

API menyediakan `GET /api/db/{id}/diff/options` untuk pilihan schema, `POST /api/db/{id}/diff` untuk preview, dan `POST /api/db/{id}/diff/drafts` untuk menyimpan kandidat. `POST` memerlukan token CSRF. Hak View mencakup preview, sedangkan penyimpanan draft memerlukan hak Edit.

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

Server menyediakan halaman dan endpoint M2 dengan interface `Workflow`, `Leaders`, dan `Jobs`. Pasang implementasi layanan pada `httpx.Server` sebelum `Routes()` dipanggil saat startup. Lihat [kontrak Go](internal/contracts/platform.go) dan [entry point server](cmd/server/main.go).

| Layanan | Tanggung jawab integrasi |
|---|---|
| `Workflow` | Menyimpan draft dan revisi, mengelola pengajuan serta approval, memasukkan job secara persisten, dan mencatat setiap attempt pada history. Approval terkait dengan revisi dan cakupan yang ditinjau. |
| `Leaders` | Menetapkan leader dari anggota group. Workflow memeriksa approval leader dari group yang memiliki akses ke database target, termasuk View Only. |
| `Jobs` | Menyediakan status dan progress job. Resume dan kompensasi menghasilkan pengajuan pemulihan untuk ditinjau. |

Adapter memeriksa grant sebelum memanggil layanan. Enqueue memerlukan header `Idempotency-Key` dan mengembalikan `202` beserta job ID setelah job tersimpan. Worker menjalankan SQL secara asynchronous serta memeriksa ulang izin, revisi, approval, dan checkpoint. Daftar route, termasuk `/api/auth`, `/api/admin`, `/api/db/{id}/migrations`, `/api/db/{id}/requests`, dan `/api/db/{id}/jobs`, ada di [router API](internal/httpx/api.go).

## Observability

```powershell
docker compose --profile observability up -d otel-collector loki tempo prometheus grafana
$env:OTEL_EXPORTER_OTLP_ENDPOINT = 'http://127.0.0.1:4318'
$env:OTEL_EXPORTER_OTLP_INSECURE = 'true'
go run ./cmd/server
```

Masuk dengan akun `admin` dan password `GRAFANA_ADMIN_PASSWORD`. Dashboard **IF3110 - Queue, Migration & Pool** dimuat bersama tiga datasource. Collector meneruskan log ke Loki dan trace ke Tempo. Prometheus mengambil metrik request HTTP dan pool database dari `/metrics`. Panel queue dan batch mulai terisi setelah worker mengekspor metrik. [Buka Grafana](http://127.0.0.1:3000) atau lihat [konfigurasi Prometheus](deploy/observability/prometheus.yaml) dan [dashboard Grafana](deploy/observability/grafana/dashboards/platform.json).

## CodeQL

[Workflow CodeQL](.github/workflows/codeql.yml) memindai Go dan JavaScript/TypeScript dengan suite `security-extended`. GitHub Actions menyimpan hasil SARIF sebagai artifact per bahasa. Scan lokal 20 November 2026 menggunakan CodeQL CLI 2.27.5.

| Bahasa | Temuan awal | Scan ulang |
|---|---:|---:|
| Go | 10 | 3 |
| JavaScript/TypeScript | 0 | 0 |

Tujuh temuan Go diperbaiki dengan validasi ukuran nilai sebelum konversi integer pada parameter Argon2 dan sanitasi data yang ditulis ke log. Tiga temuan `go/sql-injection` dengan skor keamanan 8.8 tetap terlihat pada hasil scan.

| Lokasi | Hasil peninjauan |
|---|---|
| `internal/migration/mariadb.go:376` | Executor menjalankan DDL yang diberikan sebagai migration. Validasi membatasi jenis statement dan scope database. |
| `internal/migration/mariadb.go:388` | Executor menjalankan DML migration dalam transaksi InnoDB setelah validasi jenis statement dan scope database. |
| `internal/schemadiff/mariadb.go:60` | Query katalog memakai nama schema dan tabel dengan quoting identifier. Fungsi `qualified` menggandakan backtick di dalam nama. Temuan ini dinilai false positive. [Aturan identifier MariaDB](https://mariadb.com/docs/server/reference/sql-structure/sql-language-structure/identifier-names). |

Saat menghubungkan worker, uji kembali validasi grant dan approval pada jalur eksekusi. Bukti scan lokal tersimpan di `artifacts/codeql/` sebagai `go.sarif`, `go-verified.sarif`, dan `javascript-release.sarif`. Untuk scan CI, unduh artifact dari run yang sesuai dengan commit yang diperiksa. [Dokumentasi CodeQL](https://docs.github.com/en/code-security/how-tos/find-and-fix-code-vulnerabilities/configure-code-scanning/configuring-advanced-setup-for-code-scanning).

## Pengujian

Jalankan PostgreSQL dan MariaDB khusus pengujian agar suite integrasi mencakup kedua DBMS.

```powershell
docker compose up -d --wait test-db mariadb-test
$env:TEST_DATABASE_URL = 'postgres://test:test@localhost:5435/test?sslmode=disable'
$env:TEST_MARIADB_DSN = 'root:test@tcp(127.0.0.1:3308)/'
go vet ./...
go test -race ./... -count=1
```

Pada Bash, gunakan `export TEST_DATABASE_URL='postgres://test:test@localhost:5435/test?sslmode=disable'` dan `export TEST_MARIADB_DSN='root:test@tcp(127.0.0.1:3308)/'`. Race detector memerlukan dukungan CGO dan compiler C pada platform yang digunakan.

Jika `TEST_DATABASE_URL` atau `TEST_MARIADB_DSN` kosong, test integrasi terkait akan dilewati. Gunakan instance khusus test karena helper membuat ulang database atau schema bernama `it_*`. Suite memerlukan hak `CREATE DATABASE` dan akses admin pada instance MariaDB untuk menyiapkan akun fixture.

| Pemeriksaan lokal | Hasil |
|---|---|
| Regresi Go dengan race detector | 210 test dan subtest lolos dengan PostgreSQL dan MariaDB aktif. Cakupan termasuk rotasi session paralel, CSRF, izin akses, explorer, executor M1, dan schema diff. |
| API HTTPS dan browser desktop/mobile | 38 kasus API dan 39 pemeriksaan browser pada backend nyata lolos. Cakupan termasuk login, admin, akses database, pagination, diff, dan EXPLAIN. |
| Kontrak UI migration M2 | 20 pemeriksaan UI lolos dengan respons fixture untuk workflow dan job. Pengujian job nyata memerlukan integrasi layanan domain dan worker. |
| Build dan observability | Build SPA dan Go, `go vet`, smoke test Docker, pengambilan metrik Prometheus, log Loki, dan trace Tempo lolos. |

## Perintah Makefile

| Perintah | Fungsi |
|---|---|
| `make run` | Menyiapkan environment dan database, membangun SPA, lalu menjalankan server |
| `make db-up` | Menjalankan seluruh database Compose dan menunggu healthcheck |
| `make build` | Membangun frontend lalu seluruh package Go |
| `make frontend` | npm ci dan build SPA |
| `make observability-up` | Menjalankan profile observability dan database |
| `make observability-down` | Menghentikan lima service observability |
| `make vet` | Menjalankan `go vet ./...` |
| `make test` | Menjalankan suite Go dengan database pengujian sesuai environment |
| `make test-integration` | Menjalankan test bernama `Integration` pada package auth dan HTTP |

`make` bersifat opsional. Perintah Go dan Docker pada bagian sebelumnya dapat dijalankan langsung.

## Struktur Repository

```text
cmd/
  server/          Entry point, wiring, startup, dan graceful shutdown
  envinit/         Membuat .env dengan key acak (dipakai make run)
  hash-password/   Utilitas Argon2id; membaca password dari stdin
internal/
  admin/           Operasi admin dan bootstrap akun pertama
  auth/            Password, token, session, rotasi, dan CSRF
  authz/           Policy dan perhitungan hak akses database
  config/          Konfigurasi environment
  dbconn/          Enkripsi kredensial dan registry koneksi target
  explorer/        Pembacaan schema, struktur, dan data
  contracts/       DTO dan interface workflow/jobs/leaders M2
  httpx/           API JSON, SPA fallback, middleware, handler legacy, dan test HTTP
  observability/   Provider log/trace serta HTTP/pool metrics
  migration/       Draft, executor, dan riwayat migration
  platformdb/      Migrasi schema Platform DB
  reqctx/          Principal dan CSRF token dalam request context
  schemadiff/      Snapshot katalog, perbandingan schema, dan SQL kandidat
  testdb/          Helper PostgreSQL untuk integration test
  user/            Repository pengguna
frontend/          React/TypeScript SPA dan lockfile
deploy/observability/ Collector, Loki, Tempo, Prometheus, Grafana
.github/           Workflow CI/CodeQL
scripts/           Seed data Target DB
web/
  templates/       Go HTML templates
  static/          CSS lama dan hasil build SPA yang diabaikan Git
.env.example       Template konfigurasi development
docker-compose.yml Database development/test dan profile observability
Dockerfile         Build SPA dan binary dalam container non-root
Makefile           Perintah development
```
