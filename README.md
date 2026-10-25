# IF3110 - Database Management Platform

Tugas Besar Milestone 1 mata kuliah IF3110 Pengembangan Aplikasi Web. Aplikasi ini mengatur akses pengguna ke database PostgreSQL dan MariaDB, menyediakan explorer schema dan data, serta mencatat perubahan database melalui migration SQL. Metadata platform tetap disimpan di PostgreSQL.

Alur utama: **Login → Pilih database → Lihat schema/data → Buat draft migration → Eksekusi → Commit/Rollback → Lihat riwayat**.

## Anggota Kelompok

| NIM | Nama |
|---|---|
| 13524052 | Raynard Fausta |
| 13524085 | Ariel Cornelius Sitorus |
| 13524112 | Richard Samuel Simanullang |

## Teknologi

| Komponen | Teknologi |
|---|---|
| Backend | Go 1.25.4+, `net/http` |
| Frontend | Go `html/template`, HTML, CSS |
| Database platform | PostgreSQL 16 |
| Database target | PostgreSQL 16 dan MariaDB 10.6.1+ (Compose: 12.2) |
| Driver database | pgx v5 dan go-sql-driver/mysql |
| Password hashing | Argon2id |
| Enkripsi password koneksi | AES-256-GCM |
| Lingkungan database lokal | Docker Compose |

## Fitur

- Login dengan username dan password, logout, serta bootstrap Platform Admin pertama.
- Session di backend dengan idle timeout, absolute expiration, rotasi identifier, dan grace period untuk request bersamaan.
- Perlindungan CSRF, middleware autentikasi/admin, security headers, dan pembatasan percobaan login.
- Pengelolaan pengguna: membuat akun, mengubah status aktif/admin, dan mereset password.
- Pengelolaan group, keanggotaan, dan hak akses database melalui group.
- Pengelolaan koneksi PostgreSQL dan MariaDB: tambah, ubah, hapus, dan test connection. Password koneksi disimpan terenkripsi. Jenis DBMS dipilih saat membuat koneksi dan tidak dapat diubah pada koneksi yang sama.
- Dashboard database sesuai hak akses pengguna.
- Explorer schema, tabel, kolom, constraint, index, dan data dengan pagination 50 baris per halaman.
- Pembuatan dan pengubahan draft migration, eksekusi SQL dalam transaksi, serta riwayat hasil eksekusi.
- Schema diff terhadap schema referensi dan pembuatan draft SQL migration yang dapat ditinjau serta diedit sebelum eksekusi.
- Unit test, integration test dengan PostgreSQL, dan blackbox HTTP/HTTPS.

## Menjalankan Aplikasi

### 1. Prasyarat

Siapkan Go 1.25.4 atau lebih baru, Docker dengan Docker Compose v2, dan Git. Jalankan seluruh perintah berikut dari root repository.

```powershell
go mod download
Copy-Item .env.example .env
docker compose up -d --wait
```

Pada Bash, gunakan `cp .env.example .env` untuk menyalin konfigurasi.

Compose menyediakan database berikut. Server Go dijalankan langsung di komputer host.

| Service | Port host | Database | User | Password lokal |
|---|---|---|---|---|
| `platform-db` | 5433 | `platform` | `platform` | `platform` |
| `target-db` | 5434 | `target` | `target` | `target` |
| `test-db` | 5435 | `test` | `test` | `test` |
| `mariadb-target` | 3307 | `target` | `target` | `target` |
| `mariadb-test` (profile `test`) | 3308 | `test` | `root` | `test` |

Kredensial pada tabel merupakan konfigurasi development di [docker-compose.yml](docker-compose.yml). Platform DB menyimpan user, group, grant, session, koneksi, dan riwayat migration. Target PostgreSQL berisi contoh data penerbangan pada schema `flight_ops` dan `maintenance`; target MariaDB berisi tabel `airports` dan `flights` pada database `target`. Kedua Test DB menggunakan tmpfs sehingga datanya bersifat sementara. Jalankan service `mariadb-test` secara eksplisit untuk pengujian MariaDB.

### 2. Isi konfigurasi dan buat admin pertama

Edit `.env` berdasarkan [.env.example](.env.example). Untuk mencoba seluruh alur, isi:

- `PLATFORM_DATABASE_URL` sesuai Platform DB; nilai contoh sudah sesuai Compose.
- `SESSION_KEY` dan `ENCRYPTION_KEY`, masing-masing berupa base64 dari 32 byte acak.
- `BOOTSTRAP_ADMIN_USERNAME` dan `BOOTSTRAP_ADMIN_PASSWORD` untuk akun admin pertama. Password minimal 8 karakter ASCII.

Gunakan fungsi PowerShell berikut untuk menghasilkan dua key yang berbeda, lalu salin hasilnya ke `.env`:

```powershell
function New-AppKey {
    $bytes = New-Object byte[] 32
    $rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $rng.GetBytes($bytes)
        [Convert]::ToBase64String($bytes)
    } finally {
        $rng.Dispose()
    }
}

"SESSION_KEY=$(New-AppKey)"
"ENCRYPTION_KEY=$(New-AppKey)"
```

Pada Bash, jalankan `openssl rand -base64 32` dua kali dan gunakan masing-masing hasil untuk kedua key tersebut.

Bootstrap membuat akun hanya jika belum ada Platform Admin dan username belum dipakai. Tidak ada akun login bawaan. Setelah akun berhasil dibuat, kosongkan kedua variabel `BOOTSTRAP_ADMIN_*` pada `.env` dan environment terminal sebelum startup berikutnya.

### 3. Jalankan server

Saat startup, server memuat `.env` dari direktori kerja secara otomatis. Variabel environment yang sudah di-set di terminal tidak ditimpa. Dengan `APP_ENV=production`, `.env` tidak dibaca dan semua variabel wajib di-set langsung di environment. Jalankan dari root repository:

```bash
go run ./cmd/server
```

Migrasi schema Platform DB dijalankan otomatis saat startup. Buka **http://localhost:8080/login** lalu masuk menggunakan akun bootstrap. Endpoint **http://localhost:8080/healthz** memeriksa koneksi Platform DB.

Untuk menghentikan aplikasi, tekan `Ctrl+C`. Untuk menghentikan database, jalankan `docker compose down`; volume Platform DB dan Target DB tetap disimpan.

## Konfigurasi Environment

| Variabel | Default/contoh | Kegunaan |
|---|---|---|
| `PLATFORM_DATABASE_URL` | `postgres://platform:platform@localhost:5433/platform?sslmode=disable` | Wajib; koneksi Platform DB |
| `HTTP_ADDR` | `:8080` | Alamat server HTTP |
| `APP_ENV` | `development` | `development`, `test`, atau `production` |
| `APP_ORIGIN` | `http://localhost:8080` pada `.env.example` | Origin publik untuk pemeriksaan CSRF, tanpa path atau trailing slash; wajib HTTPS pada production |
| `SESSION_KEY` | Base64 dari 32 byte acak | Key CSRF pre-session dan pemulihan token hasil rotasi; wajib pada production |
| `ENCRYPTION_KEY` | Base64 dari 32 byte acak | Key enkripsi password koneksi target; wajib untuk mengelola koneksi dan pada production |
| `BOOTSTRAP_ADMIN_USERNAME` | Kosong | Username admin pertama |
| `BOOTSTRAP_ADMIN_PASSWORD` | Kosong | Password admin pertama |
| `SESSION_IDLE_TIMEOUT` | `30m` | Batas session tanpa aktivitas |
| `SESSION_ABSOLUTE_TTL` | `8h` | Umur maksimum session sejak login |
| `SESSION_ROTATION_INTERVAL` | `15m` | Interval rotasi identifier |
| `SESSION_GRACE_PERIOD` | `30s` | Masa berlaku identifier sebelumnya setelah rotasi |
| `TEST_DATABASE_URL` | `postgres://test:test@localhost:5435/test?sslmode=disable` | Koneksi instance PostgreSQL khusus pengujian |
| `TEST_MARIADB_DSN` | `root:test@tcp(127.0.0.1:3308)/` | Opsional; instance MariaDB khusus integration test, diisi melalui environment |

Durasi menggunakan format Go, misalnya `30m` atau `8h`. Konfigurasi harus memenuhi `0 < grace < rotation < absolute` dan `0 < idle <= absolute`.

Di development, `SESSION_KEY` kosong menghasilkan key sementara saat startup. Menetapkan key yang stabil menjaga validitas CSRF pre-session dan pemulihan cookie rotasi antar-restart. `ENCRYPTION_KEY` kosong masih memungkinkan login, tetapi koneksi target tidak dapat dibuat atau dibuka. Simpan key enkripsi yang sama agar password koneksi yang sudah tersimpan tetap bisa didekripsi.

Pada production, gunakan reverse proxy HTTPS, `APP_ENV=production`, origin HTTPS publik, serta key yang sama pada seluruh instance aplikasi. **Commit `.env.example`, bukan `.env`**; `.env` sudah tercantum pada `.gitignore`.

## Alur Penggunaan

1. Login sebagai Platform Admin.
2. Buka **Pengguna** untuk membuat akun pengguna.
3. Buka **Group & anggota**, buat group, lalu tambahkan pengguna sebagai anggota.
4. Buka **Koneksi database**, pilih DBMS **PostgreSQL**, dan daftarkan Target DB contoh:

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
8. Pilih **Buka explorer** untuk melihat schema/data atau **Riwayat migration** untuk melihat migration.
9. Dengan akses `edit`, buat draft, isi nama dan SQL, simpan, lalu eksekusi. Contoh pada database development:

   ```sql
   CREATE TABLE flight_ops.demo_notes (
       id bigserial PRIMARY KEY,
       note text NOT NULL
   );
   ```

10. Periksa status dan detail migration. SQL sukses di-commit; SQL gagal di-rollback dan hasilnya dicatat pada riwayat.

Platform Admin tetap memerlukan keanggotaan group dan grant untuk membuka database. Tambahkan akun admin ke group bila admin juga ingin menggunakan explorer atau menjalankan migration.

### Target MariaDB (bonus DBMS tambahan)

Pada **Koneksi database**, pilih `MariaDB`, host `localhost`, port `3307`, database `target`, username/password `target`, dan SSL `disable` untuk target Compose lokal. Pada explorer MariaDB, database koneksi ditampilkan sebagai schema. Grant View/Edit dan pemeriksaan backend berlaku sama seperti PostgreSQL. Gunakan akun target yang haknya dibatasi pada database tersebut.

SSL MariaDB mendukung `disable`, `require` (enkripsi tanpa verifikasi sertifikat), dan `verify-full` (verifikasi sertifikat serta hostname). MariaDB sebelum 10.6.1 dan DBMS yang tidak didukung ditolak saat test/open connection. Koneksi PostgreSQL yang sudah ada tetap bertipe PostgreSQL setelah migrasi platform otomatis `0002_connection_dbms`.

Aturan migration MariaDB:

- Satu migration schema berisi **tepat satu statement DDL** tabel/index/view. Contoh: `ALTER TABLE flights ADD COLUMN gate VARCHAR(10);`.
- DDL MariaDB melakukan implicit commit. Batch beberapa DDL atau campuran DDL-DML ditolak saat menyimpan dan sebelum eksekusi; aplikasi tidak menjanjikan rollback transaksi untuk DDL.
- Batch `INSERT`, `UPDATE`, `DELETE`, `REPLACE`, dan `SELECT` dijalankan dalam transaksi. Database yang mengandung tabel non-InnoDB ditolak untuk batch DML dan analisis plan.
- Kontrol transaksi, perintah administrasi, executable comments, `CREATE OR REPLACE TABLE`, `CREATE TABLE AS SELECT/LIKE`, DDL partisi, serta DROP/RENAME beberapa objek tidak didukung. Referensi ke database lain ditolak.
- Execution plan DML ditampilkan sebagai JSON native dari `EXPLAIN FORMAT=JSON` atau `ANALYZE FORMAT=JSON`. Analisis selalu di-rollback. DDL tidak dianalisis karena dapat melakukan implicit commit.
- Riwayat dan penomoran versi tetap dicatat pada Platform DB. Jika server/koneksi terputus saat eksekusi, periksa kondisi Target DB sebelum membuat migration perbaikan.

Perbedaan transaksi ini mengikuti [implicit commit MariaDB](https://mariadb.com/docs/server/reference/sql-statements/transactions/sql-statements-that-cause-an-implicit-commit) dan [atomic DDL MariaDB](https://mariadb.com/docs/server/reference/sql-statements/data-definition/atomic-ddl).

## Aturan Akses dan Session

### Schema diff dan migration generation (bonus)

1. Buka **Schema diff** dari Explorer Schema atau Riwayat Migration pada database aktif.
2. Pilih koneksi referensi yang berisi kondisi schema yang diinginkan, lalu klik **Muat pilihan schema**. Referensi dapat berupa schema lain pada koneksi yang sama atau koneksi lain dengan DBMS yang sama.
3. Pilih schema aktif dan schema referensi, lalu klik **Bandingkan dan buat SQL kandidat**. Kedua koneksi memerlukan hak View.
4. Tinjau perubahan dan SQL. Perbandingan mencakup tabel biasa, kolom bertipe builtin, default, nullability, primary/unique/foreign key, check, serta index BTREE biasa. Foreign key lokal dipetakan ke schema aktif dan SQL diurutkan sesuai dependensi.
5. Dengan hak Edit pada database aktif, beri nama lalu simpan draft. Perubahan yang menghapus tabel/kolom atau mengubah tipe memerlukan pengakuan risiko pada form. Rename tidak ditebak: nama yang hilang/baru dianggap hapus/tambah.
6. PostgreSQL membuat satu draft gabungan dan membuka editor. MariaDB membuat draft terpisah untuk tiap statement DDL dan menampilkan urutan pengerjaannya. Tinjau/edit setiap draft sebelum mengeksekusinya melalui alur migration biasa.
7. Setelah eksekusi selesai, bandingkan kembali untuk memeriksa kondisi schema.

Preview dan penyimpanan draft tidak menjalankan DDL pada target maupun referensi. Server menghitung ulang SQL dari katalog, memeriksa ulang hak akses, dan menolak penyimpanan jika fingerprint schema berubah sejak preview (`409`). Fingerprint mencakup metadata yang dibandingkan, bukan data tabel atau nilai counter sequence. Batch draft disimpan secara atomik pada Platform DB; hal ini tidak membuat seluruh batch DDL MariaDB menjadi satu transaksi.

Objek/atribut khusus seperti view, partition/inheritance, trigger, kolom generated, tipe/domain/enum khusus, expression/partial index, custom operator class/collation, opsi sequence khusus, dan foreign key lintas schema memerlukan penanganan manual. Ketika metadata tersebut menghalangi perbandingan yang dapat digenerasikan, tombol simpan otomatis dinonaktifkan. Perbandingan bukan salinan data atau seluruh objek database: komentar, owner, grant, definisi fungsi, serta sequence mandiri berada di luar cakupan. Komentar kolom MariaDB yang sudah ada dipertahankan ketika kolom diubah.

Tidak ada `DROP CASCADE` otomatis. Konversi tipe, NOT NULL, dan constraint baru tetap dapat gagal jika data aktif tidak memenuhi syarat; tinjau data dan sesuaikan SQL pada editor. Batas satu perbandingan adalah 200 tabel, 2000 kolom, dan 250 statement.

Endpoint fitur: `GET/POST /db/{id}/schema/diff` untuk form/preview dan `POST /db/{id}/schema/diff/draft` untuk penyimpanan. POST tetap memerlukan CSRF; View Only dapat membandingkan tetapi tidak menyimpan draft.

### Hak akses database

| Kemampuan | Tanpa grant | View Only (`view`) | View + Edit (`edit`) |
|---|---|---|---|
| Melihat schema, struktur tabel, dan data | Ditolak | Diizinkan | Diizinkan |
| Melihat riwayat dan detail migration | Ditolak | Diizinkan | Diizinkan |
| Membuat dan mengubah draft migration | Ditolak | Ditolak | Diizinkan |
| Menjalankan migration | Ditolak | Ditolak | Diizinkan |

Akses diberikan melalui group. Jika pengguna tergabung dalam beberapa group, akses efektif pada setiap database adalah level tertinggi: `edit > view > none`. Status Platform Admin mengizinkan pengelolaan platform, tetapi tidak otomatis memberi akses database. Pemeriksaan hak akses dilakukan di backend pada setiap aksi database.

### Autentikasi dan CSRF

- Password menggunakan Argon2id dengan salt acak; username tidak membedakan huruf besar/kecil.
- Login berhasil menghasilkan identifier session opaque 256 bit. Backend hanya menyimpan hash SHA-256 identifier; client menerimanya melalui cookie.
- Session berakhir setelah idle 30 menit atau mencapai umur 8 jam. Rotasi berlangsung tiap 15 menit dan ketika privilege ditingkatkan, dengan grace period 30 detik. Rotasi tidak memperpanjang absolute expiration.
- Compare-and-swap memastikan request bersamaan menggunakan hasil rotasi yang sama. CSRF token session tetap berlaku saat identifier dirotasi.
- Cookie memakai `HttpOnly`, `SameSite=Lax`, serta `Secure` pada HTTPS/production. Production menggunakan nama cookie `__Host-session`.
- Logout, penonaktifan akun, dan reset password mencabut session di backend. Policy database dibaca ulang sehingga perubahan grant berlaku pada request berikutnya.
- Request yang mengubah state memerlukan `csrf_token` pada body form atau header `X-CSRF-Token`. Login menggunakan signed double-submit cookie pre-session dengan masa berlaku 10 menit. Token dari query URL tidak diterima.
- Login dibatasi 10 percobaan per IP per menit per proses, dengan maksimal 4 verifikasi password bersamaan.

## Aturan Migration

- Draft belum memiliki nomor versi dan masih dapat diubah. Saat dieksekusi, versi berikutnya dihitung per database dan status berubah menjadi `running`, lalu `success` atau `failed`.
- Migration yang sudah dieksekusi tidak dapat diubah atau dijalankan ulang pada baris yang sama. Perbaikan dilakukan dengan membuat draft baru dari migration sebelumnya.
- SQL PostgreSQL dijalankan dalam satu transaksi Target DB dengan lock timeout 5 detik dan statement timeout 60 detik. MariaDB menggunakan aturan DDL/DML pada bagian bonus di atas, dengan named lock per database dan batas waktu eksekusi.
- Perintah pengendali transaksi seperti `BEGIN`/`COMMIT` serta perintah yang tidak dapat dijalankan dalam transaksi, seperti `VACUUM` dan `CREATE INDEX CONCURRENTLY`, ditolak.
- Platform DB dan Target DB menggunakan transaksi terpisah. Jika server berhenti ketika migration berjalan, startup berikutnya menandainya `failed`; kondisi Target DB perlu diperiksa sesuai pesan pada riwayat.

## Pengujian

Untuk menjalankan seluruh test, pastikan Test DB aktif dan `TEST_DATABASE_URL` sudah dimuat:

```powershell
docker compose up -d --wait test-db
docker compose up -d --wait mariadb-test
$env:TEST_DATABASE_URL = 'postgres://test:test@localhost:5435/test?sslmode=disable'
$env:TEST_MARIADB_DSN = 'root:test@tcp(127.0.0.1:3308)/'
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
```

Pada Bash, gunakan `export TEST_DATABASE_URL='postgres://test:test@localhost:5435/test?sslmode=disable'` dan `export TEST_MARIADB_DSN='root:test@tcp(127.0.0.1:3308)/'`. Race detector memerlukan dukungan CGO dan compiler C pada platform yang digunakan.

Jika `TEST_DATABASE_URL` atau `TEST_MARIADB_DSN` kosong, test yang membutuhkan instance terkait akan di-skip. Helper pengujian membuat ulang database/schema bernama `it_*` dan membutuhkan hak `CREATE DATABASE`; gunakan instance khusus test. Blackbox multi-DBMS juga membuat user uji MariaDB dengan grant terbatas pada schema fixture sehingga membutuhkan akun admin pada instance test.

| Area | Cakupan pengujian |
|---|---|
| Auth dan session | Password benar/salah, user nonaktif, idle/absolute timeout, rotasi, grace, CAS, request paralel, dan revoke |
| Middleware dan CSRF | Autentikasi/admin, token valid/tidak valid, Origin, cookie flags, rate limit, security headers, dan penanganan kegagalan |
| Blackbox HTTP/HTTPS | Login sampai logout, replay identifier, expiry, CSRF, dan 96 request paralel saat rotasi |
| Admin dan otorisasi | Bootstrap admin, pengguna, group/anggota, grant efektif, matriks akses, serta akses yang ditolak |
| Koneksi | Enkripsi/dekripsi, validasi key, registry, dan test connection |
| Explorer dan migration | Schema/data, pagination, validasi SQL, eksekusi sukses/gagal, rollback, versi, dan riwayat |
| Multi-DBMS | Upgrade metadata lama, driver/cache, scope database, explorer MariaDB, DDL/DML, rollback InnoDB, plan native, dan blackbox HTTPS untuk kedua DBMS |
| Schema diff | Diff deterministik, quote identifier, urutan dependensi, round-trip DDL pada kedua DBMS, preservasi data, metadata khusus, drift fingerprint, CSRF/hak akses, draft yang dapat diedit, serta atomic batch save |

## Perintah Makefile

| Perintah | Fungsi |
|---|---|
| `make run` | Membuat `.env` jika belum ada (kunci acak + admin `admin`, password lihat `BOOTSTRAP_ADMIN_PASSWORD` di `.env`), menyalakan database, lalu menjalankan server |
| `make db-up` | Menjalankan seluruh database Compose dan menunggu healthcheck |
| `make build` | Memeriksa build seluruh package |
| `make vet` | Menjalankan `go vet ./...` |
| `make test` | Menjalankan seluruh test; integration mengikuti `TEST_DATABASE_URL` |
| `make test-integration` | Menjalankan test bernama `Integration` pada package auth dan HTTP |

`make` bersifat opsional. Perintah Go dan Docker pada bagian sebelumnya dapat dijalankan langsung.

## Struktur Repository

```text
cmd/
  server/          Entry point, wiring, startup, dan graceful shutdown
  hash-password/   Utilitas Argon2id; membaca password dari stdin
internal/
  admin/           Operasi admin dan bootstrap akun pertama
  auth/            Password, token, session, rotasi, dan CSRF
  authz/           Policy dan perhitungan hak akses database
  config/          Konfigurasi environment
  dbconn/          Enkripsi kredensial dan registry koneksi target
  explorer/        Pembacaan schema, struktur, dan data
  httpx/           Routing, middleware, handler, rendering, dan test HTTP
  migration/       Draft, executor, dan riwayat migration
  platformdb/      Migrasi schema Platform DB
  reqctx/          Principal dan CSRF token dalam request context
  schemadiff/      Snapshot katalog, perbandingan schema, dan SQL kandidat
  testdb/          Helper PostgreSQL untuk integration test
  user/            Repository pengguna
scripts/           Seed data Target DB
web/
  templates/       Go HTML templates
  static/          CSS
.env.example       Contoh konfigurasi tanpa key atau password admin
docker-compose.yml Lingkungan database development dan test
Makefile           Perintah development
```
