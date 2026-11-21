# Pengujian performa

## Dataset migration besar

`dataset.sql` membuat ulang `perf.accounts` (kolom `id` unik dan tidak berubah untuk cursor batch)
sehingga setiap pengujian, baik mode biasa maupun batch, dimulai dari keadaan awal yang sama.
`calibrate.sh` membuat dataset lalu mengukur **satu** UPDATE seluruh row tanpa batching:

```sh
docker compose up -d --wait target-db
ROWS=2000000 sh tests/performance/calibrate.sh
```

Query yang diukur:

```sql
UPDATE perf.accounts SET balance = balance + 1, updated_at = now();
```

| Row | Ukuran tabel | Durasi UPDATE tanpa batch |
|---:|---:|---:|
| 500.000 | tidak dicatat | 1,30 s (terlalu dekat dengan batas 1 s) |
| **2.000.000** | 158 MB (sebelum UPDATE) | **6,16 s dan 6,69 s** (dua kali percobaan) |

Dataset uji yang dipakai: **2.000.000 row** (>1 detik dengan margin besar, tanpa sleep buatan).
Gunakan dataset yang sama untuk migration per batch, misalnya batch size 10.000 dengan cursor `id`.

Lingkungan: PostgreSQL 16.15 (`postgres:16-alpine`) di Docker Desktop (WSL2), 16 vCPU, RAM 7,6 GB
untuk VM Docker, Windows 11. Diukur 21 November 2026.

## Stress test k6 (`stress.js`)

Dua skenario berjalan bersamaan dengan tahapan beban yang sama:

| Tahap | Durasi | VU API | VU job |
|---|---|---:|---:|
| Normal (baseline tim) | 1m | 10 | 2 |
| 3x normal | 1m | 30 | 6 |
| 6x normal | 1m | 60 | 12 |
| Turun: pemulihan | 1m | 10 | 2 |
| Selesai | 30s | 0 | 0 |

- **api**: dashboard, daftar migration, history (Hide rollback), dan daftar job.
- **jobs**: membuat migration → mengajukan → submit → approve oleh leader → enqueue → polling job
  sampai status terminal. Database bergantian antara `DB_SAME` dan `DB_OTHER`, sehingga job pada
  database sama (serial) dan berbeda (paralel) diuji bersamaan. Respons 202 tidak dihitung sebagai
  keberhasilan; `job_success` hanya dihitung dari status akhir job.

Metrik: `http_req_duration` p50/p95 (skenario api), `http_reqs` (throughput), `http_req_failed`
(error rate), `job_queue_wait_ms` (resolusi = interval polling 1 s), `job_duration_ms`, serta
`jobs_completed` (count dan rate = throughput penyelesaian job).

Threshold: API p50 < 200 ms, **p95 < 500 ms**, **error rate < 1%**, `job_success` > 95%.

Persiapan: satu pengguna dengan grant edit ke kedua database dan menjadi leader dari setiap group
yang memiliki grant ke database tersebut. Kedua database diisi `dataset.sql`.

```sh
k6 run -e BASE_URL=http://localhost:8080 -e USERNAME=perf -e PASSWORD=... \
       -e DB_SAME=<uuid-koneksi-1> -e DB_OTHER=<uuid-koneksi-2> tests/performance/stress.js
```

Ringkasan tersimpan di `tests/performance/results/summary.json`.

### Hasil

Belum diukur. Eksekusi job memerlukan modul queue/worker. Tanpa modul itu, enqueue membalas
`503 module_unavailable`. Tabel hasil dan analisis per tingkat beban diisi setelah worker terintegrasi.
