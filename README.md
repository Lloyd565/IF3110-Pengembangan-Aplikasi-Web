# IF3110 - Database Management Platform

Tugas Besar IF3110 Pengembangan Aplikasi Web. Aplikasi ini mengatur akses ke database target, menyediakan penjelajahan schema dan data, serta membantu pengguna meninjau perubahan schema. PostgreSQL menyimpan metadata platform, termasuk pengguna, session, group, grant, dan koneksi target.

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

Server memasang layanan workflow, leader group, dan job pada startup. Proses worker terpisah menjalankan migration PostgreSQL dan MariaDB dari queue persisten. Rincian layanan ada di [Integrasi Migration M2](#integrasi-migration-m2).

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

Di development, `SESSION_KEY` kosong menghasilkan key sementara saat startup. Menetapkan key yang stabil menjaga validitas CSRF pre-session dan pemulihan cookie rotasi antar-restart. `ENCRYPTION_KEY` kosong masih memungkinkan login, tetapi koneksi target tidak dapat dibuat atau dibuka. Key enkripsi yang tetap memungkinkan dekripsi password koneksi yang tersimpan.

Konfigurasi production memakai reverse proxy HTTPS, `APP_ENV=production`, origin HTTPS publik, serta key yang sama pada seluruh instance aplikasi. Mode ini membaca konfigurasi dari environment proses. `.env.example` memuat contoh konfigurasi, sedangkan `.env` memuat kredensial development dan tercantum pada `.gitignore`.

## Alur Penggunaan

Akun Platform Admin mengelola pengguna, group, keanggotaan, koneksi, dan grant melalui menu administrasi. Koneksi PostgreSQL development memakai host `localhost`, port `5434`, database/user/password `target`, dan SSL `disable`. Seed pada [scripts/target-seed.sql](scripts/target-seed.sql) dimuat ketika volume Target DB pertama kali dibuat.

Anggota group dengan grant View melihat database pada dashboard dan membuka schema, data, history, serta preview schema diff. Grant Edit menyediakan pembuatan draft dan pengajuan migration. Workspace memuat tab Schema, Migration, Review & approval, History, Job, dan Schema diff.

Draft migration memuat SQL up serta SQL down atau alasan irreversible. Pengajuan mengacu pada revisi tertentu dan memerlukan approval leader group sesuai akses database. Perubahan revisi memerlukan review terhadap revisi yang digunakan. Enqueue menghasilkan job ID untuk pemantauan status, attempt, baris, batch yang telah commit, dan error.

Worker memeriksa ulang izin dan approval, mengambil job dari queue persisten, dan menjalankan SQL di luar request HTTP. Pengajuan terikat pada konfigurasi koneksi dan identitas server/database target. Perubahan host, database, kredensial, TLS, atau DBMS membuat approval lama tidak berlaku untuk eksekusi. Pergantian kredensial tidak mengubah identitas target untuk pembacaan bukti pemulihan. Worker memprioritaskan pemulihan lease yang kedaluwarsa dan memakai backoff dengan batas percobaan saat bukti belum dapat dibaca. Pengajuan yang telah masuk queue tetap menerima perubahan keputusan leader; penolakan berlaku sebelum transaksi batch berikutnya. Job pada database yang sama memakai lock eksekusi, sedangkan database berbeda dapat diproses oleh worker berbeda. Rollback menggunakan SQL down dari revisi terkait. Forward dan pemulihan partial batch mengikuti pengajuan serta approval. History attempt dan status job tersimpan dalam satu transaksi metadata. Setiap klaim memiliki token kepemilikan dan lease; worker pengganti memeriksa identitas target dan autentikasi bukti sebelum melanjutkan pekerjaan. Bukti yang tidak tersedia atau tidak sah mempertahankan keadaan yang perlu direkonsiliasi. Draft lama memperoleh identitas target ketika dikirim untuk review. Pengajuan lama yang sudah ditinjau memerlukan pengajuan baru. History lama tetap tersedia.

Platform Admin tetap memerlukan keanggotaan group dan grant untuk membuka database. Akun admin yang menggunakan explorer atau migration menjadi anggota group dengan grant yang sesuai.

Cursor batch PostgreSQL memakai kolom integer `NOT NULL` dengan primary key atau unique index satu kolom yang valid dan tidak parsial. Tabel dengan inheritance, partisi, atau trigger pengguna tidak memenuhi prasyarat cursor batch. SQL menyertakan batas `cursor > $1 AND cursor <= $2` dan tidak mengubah cursor. Nilai minimum bigint ditolak karena tidak mempunyai batas bawah eksklusif yang dapat direpresentasikan oleh bigint.

Cakupan batch mengikuti high-water mark yang tersimpan saat mulai, dengan ukuran 1 sampai 100000 baris. Setiap transaksi batch mengambil lock tulis pada tabel terkait selama pemilihan rentang dan eksekusi. Insert dengan cursor di atas high-water mark tidak ikut diproses; insert pada rentang yang belum dilewati dapat ikut diproses. Baris yang telah dihapus tidak diproses. Nilai total awal merupakan jumlah baris saat cakupan dibuat, sehingga dapat berbeda dari jumlah akhir ketika ada perubahan eksternal. Perubahan cursor oleh transaksi eksternal tidak memenuhi asumsi cursor stabil; cakupan ini memakai keanggotaan baris yang tersedia saat setiap batch dimulai. Batch UPDATE mencatat versi baris yang berhasil diubah dalam transaksi yang sama dengan checkpoint. Rollback hanya menjalankan SQL down pada baris yang versinya masih cocok. Baris baru di rentang lama tidak ikut berubah. Perubahan versi, penghapusan, atau checkpoint lama tanpa daftar baris memerlukan rekonsiliasi. Mode batch untuk apply dan forward hanya menerima UPDATE, karena hanya baris UPDATE yang tercatat sehingga dapat dikompensasi. DELETE dan INSERT ditolak saat pengajuan. SQL down pada rollback batch boleh berupa UPDATE atau DELETE.

### Target MariaDB (bonus DBMS tambahan)

Koneksi database MariaDB development memakai host `localhost`, port `3307`, database `target`, username/password `target`, dan SSL `disable` pada target Compose lokal. Pada explorer MariaDB, database koneksi ditampilkan sebagai schema. Grant View/Edit dan pemeriksaan backend berlaku sama seperti PostgreSQL. Akun target memakai hak yang dibatasi pada database koneksi.

SSL MariaDB mendukung `disable`, `require` (enkripsi tanpa verifikasi sertifikat), dan `verify-full` (verifikasi sertifikat serta hostname). MariaDB sebelum 10.6.1 dan DBMS yang tidak didukung ditolak saat test/open connection. Koneksi PostgreSQL yang sudah ada tetap bertipe PostgreSQL setelah migrasi platform otomatis `0002_connection_dbms`.

Explorer, schema diff, draft, approval, queue, worker, history, rollback, dan forward menyediakan dukungan MariaDB. Workflow MariaDB menggunakan mode normal untuk satu DDL atau beberapa DML InnoDB dalam satu transaksi. Cursor batch M2 tersedia pada PostgreSQL. Eksekusi MariaDB memakai aturan berikut.

- Satu migration schema berisi **tepat satu statement DDL** tabel, index, atau view, misalnya `ALTER TABLE flights ADD COLUMN gate VARCHAR(10);`.
- DDL MariaDB melakukan implicit commit. Validasi menolak beberapa DDL sekaligus dan campuran DDL-DML.
- Rangkaian `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, dan `SELECT` dijalankan dalam transaksi. Database yang mengandung tabel non-InnoDB ditolak untuk batch DML.
- Kontrol transaksi, perintah administrasi, executable comments, `CREATE OR REPLACE TABLE`, `CREATE TABLE AS SELECT/LIKE`, DDL partisi, serta DROP/RENAME beberapa objek tidak didukung. Referensi ke database lain ditolak.
- Analisis execution plan tersedia pada koneksi PostgreSQL.
- Riwayat dan penomoran versi tetap dicatat pada Platform DB. Bukti commit pada Target DB menjadi acuan pemulihan setelah server atau koneksi terputus saat eksekusi. DDL yang berstatus tidak pasti memerlukan rekonsiliasi manual. Bukti baru memakai autentikasi kriptografis berbasis key platform, identitas target, dan token job. Akun koneksi tetap memiliki privilege yang ditentukan administrator database. Akun dengan privilege administratif dipercaya oleh platform. Schema dan tabel internal tidak tampil pada explorer atau schema diff. MariaDB menyimpan token identitas database pada tabel internal saat target pertama kali diikat ke pengajuan.

Perbedaan transaksi ini mengikuti [implicit commit MariaDB](https://mariadb.com/docs/server/reference/sql-statements/transactions/sql-statements-that-cause-an-implicit-commit) dan [atomic DDL MariaDB](https://mariadb.com/docs/server/reference/sql-statements/data-definition/atomic-ddl).

### Query execution plan analysis (bonus)

Pada editor migration PostgreSQL, pengguna dengan hak Edit dapat menekan **Analisis plan SQL up**. `EXPLAIN` menampilkan estimasi biaya, jumlah baris, scan, index, join, dan filter. `EXPLAIN ANALYZE` mengukur jumlah baris dan waktu aktual hanya untuk SELECT yang lolos pemeriksaan keamanan. Analisis menggunakan transaksi read-only, timeout, function dan operator bawaan yang diizinkan, serta pemeriksaan relasi. DDL dan statement pendukung ditampilkan sebagai statement tanpa plan dan tidak dieksekusi. Query yang bergantung pada DDL sebelumnya dalam preview memerlukan schema yang sudah tersedia. View, RLS, tipe pengguna, function berpotensi mengubah state, dan bentuk query yang tidak dapat dianalisis secara aman ditolak.

### Schema diff dan migration generation (bonus)

Schema diff membandingkan schema aktif dan schema referensi pada DBMS yang sama. Referensi dapat berasal dari schema lain pada koneksi yang sama atau dari koneksi lain yang dapat diakses pengguna. Hasilnya memuat daftar perubahan, risiko, batasan manual, dan SQL kandidat.

PostgreSQL menyediakan satu draft gabungan. MariaDB menyediakan satu draft per statement sesuai urutan dependensi. Draft tetap dapat diedit sebelum eksekusi. Draft workflow M2 memerlukan SQL down atau alasan irreversible sebelum pengajuan. Penyimpanan draft tidak mengeksekusi SQL pada database target.

Server membuat preview SQL dari metadata katalog. Saat menyimpan draft, server menghitung ulang kandidat, memeriksa hak akses, dan mengembalikan `409` jika fingerprint schema berubah. Layanan `SaveGenerated` menyimpan rangkaian draft secara atomik pada database platform. Eksekusi DDL MariaDB mengikuti aturan implicit commit pada database target.

Objek khusus seperti view, partition, trigger, kolom generated, tipe kustom, partial index, dan foreign key lintas schema memerlukan penanganan manual. Data, owner, grant, fungsi, dan sequence mandiri berada di luar cakupan perbandingan. Default kolom SERIAL PostgreSQL mencakup default sequence bawaan, default pengganti, dan ketiadaan default. Pemulihan default bawaan memakai sequence milik kolom aktif. Constraint PRIMARY KEY dan UNIQUE mencakup kolom INCLUDE. Default yang bergantung pada sequence mandiri atau relasi lain memerlukan pemetaan manual dan tidak menghasilkan SQL otomatis. Penanda manual tetap ditampilkan ketika tidak ada perbedaan dalam cakupan yang didukung.

Penambahan kolom nullable sederhana dapat menghasilkan SQL ketika objek khusus pada tabel lain identik di kedua snapshot. Perubahan lainnya tetap memeriksa objek khusus dan dependensi secara konservatif. Komentar kolom MariaDB dipertahankan saat kolom diubah.

Perubahan tipe, `NOT NULL`, dan constraint baru memerlukan peninjauan SQL serta data yang ada. Batas satu perbandingan adalah 200 tabel, 2.000 kolom, dan 250 statement.

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

Server menyediakan halaman dan endpoint M2 melalui interface `Workflow`, `Leaders`, dan `Jobs`. [Entry point server](cmd/server/main.go) memasang implementasi dari package workflow dan jobs pada startup. [Kontrak Go](internal/contracts/platform.go) mendefinisikan DTO dan interface yang digunakan adapter API.

| Layanan | Tanggung jawab |
|---|---|
| `Workflow` | Menyimpan draft dan revisi, mengelola pengajuan serta approval, memasukkan job secara persisten, dan mencatat setiap attempt pada history. Approval terkait dengan revisi dan cakupan yang ditinjau. |
| `Leaders` | Menetapkan leader dari anggota group. Workflow memeriksa approval leader dari group yang memiliki akses ke database target, termasuk View Only. |
| `Jobs` | Menyediakan status dan progress job. Resume dan kompensasi menghasilkan pengajuan pemulihan untuk ditinjau. |

Adapter memeriksa grant sebelum memanggil layanan. Enqueue memerlukan header `Idempotency-Key` dan mengembalikan `202` beserta job ID setelah job tersimpan. Worker menjalankan SQL secara asynchronous serta memeriksa ulang izin, revisi, approval, dan checkpoint. Daftar route, termasuk `/api/auth`, `/api/admin`, `/api/db/{id}/migrations`, `/api/db/{id}/requests`, dan `/api/db/{id}/jobs`, ada di [router API](internal/httpx/api.go).

Worker memakai `WORKER_ID` (default hostname-PID), `WORKER_LEASE=30s`, `WORKER_POLL=1s`, `WORKER_LOCK_WAIT=30s`, `WORKER_SQL_TIMEOUT=30m`, dan `WORKER_METRICS_ADDR=:9102`. Setiap worker memakai alamat metrics yang tersedia pada hostnya.

## Observability

```powershell
docker compose --profile observability up -d otel-collector loki tempo prometheus grafana
$env:OTEL_EXPORTER_OTLP_ENDPOINT = 'http://127.0.0.1:4318'
$env:OTEL_EXPORTER_OTLP_INSECURE = 'true'
go run ./cmd/server
```

Grafana menggunakan akun `admin` dan password dari `GRAFANA_ADMIN_PASSWORD`. Dashboard **IF3110 - Queue, Migration & Pool** dimuat bersama tiga datasource. Collector meneruskan log ke Loki dan trace ke Tempo. Prometheus mengambil metrik request HTTP dan pool database dari `/metrics`. Worker mengekspor metrik queue, batch, dan pool pada port 9102. Prometheus mengambil metrik API pada port 8080 dan worker pada port 9102. [Grafana development](http://127.0.0.1:3000), [konfigurasi Prometheus](deploy/observability/prometheus.yaml), dan [dashboard Grafana](deploy/observability/grafana/dashboards/platform.json) menyediakan akses serta konfigurasi observability.

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

[Pengujian performa](tests/performance/README.md) mencakup dataset migration besar dan skenario k6 untuk API serta job sampai status terminal.

## Perintah Makefile

| Perintah | Fungsi |
|---|---|
| `make worker` | Menjalankan worker dengan environment aplikasi |
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
  worker/          Worker queue persisten dan endpoint metrics
  envinit/         Membuat .env dengan key acak (dipakai make run)
  hash-password/   Utilitas Argon2id; membaca password dari stdin
internal/
  admin/           Operasi admin dan bootstrap akun pertama
  auth/            Password, token, session, rotasi, dan CSRF
  authz/           Policy dan perhitungan hak akses database
  config/          Konfigurasi environment
  dbconn/          Enkripsi kredensial dan registry koneksi target
  explorer/        Pembacaan schema, struktur, dan data
  batch/           Cursor, checkpoint, resume, dan kompensasi per batch
  jobs/            Queue persisten dan status job
  worker/          Eksekusi asynchronous dan lease job
  workflow/        Revisi, pengajuan, approval, dan history
  telemetry/       Trace SQL dan metrik worker
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
