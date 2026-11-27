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

| Baris | UPDATE biasa | Worker batch 10.000 baris | API sampai status terminal |
|---:|---:|---:|---:|
| 2.000.000 | 9,217 s | 16,043 s (200 batch) | 16,255 s |

Dataset berisi 2.000.000 baris dan menghasilkan durasi UPDATE biasa di atas 1 detik tanpa sleep buatan. Migration batch menggunakan dataset yang sama dengan cursor `id`.

Lingkungan pengukuran adalah PostgreSQL 16.15 (`postgres:16-alpine`) pada Docker Desktop (WSL2), 16 vCPU, RAM 7,6 GB untuk VM Docker, dan Windows 11. Pengukuran berlangsung pada 25 November 2026.

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

### Pengukuran lokal

Pengukuran dilakukan pada 27 November 2026 di lingkungan berikut:

- Windows 11, 16 CPU logis, RAM 15,6 GB.
- Docker Desktop dengan 16 vCPU dan 7,6 GB RAM untuk VM.
- PostgreSQL 16.16 (`postgres:16-alpine`). `DB_SAME` = database `target`, `DB_OTHER` = database `target2`, keduanya berisi 2.000.000 baris `perf.accounts`.
- Satu proses server dan satu proses worker, dijalankan langsung di host.
- k6 0.54.0 berjalan di container (`grafana/k6:0.54.0`) dan mengakses server lewat `host.docker.internal`.
- Log JSON setiap SQL dan span (exporter console) tetap aktif, ditulis ke file.

Ringkasan seluruh run: 23.195 request, error rate 0%, API p50 16,0 ms dan p95 191,2 ms. Sebanyak 372 job selesai, semuanya `success` (`job_success` = 100%), dengan throughput 1,38 job/s. Semua threshold terpenuhi: API p50 < 200 ms, p95 < 500 ms, error rate < 1%, dan `job_success` > 95% secara agregat, ditambah p95 < 500 ms dan error rate < 1% pada setiap tahap. Hasil mentah ada di [`results/summary.json`](results/summary.json).

#### Hasil per tingkat beban

Tabel berikut dibuat dengan `python tests/performance/stages.py tests/performance/results/summary.json`. Request/s dan job/s dihitung sebagai jumlah pada tahap dibagi durasi tahap. Latency API dihitung dari skenario `api`. Request/s dan error rate mencakup kedua skenario, termasuk polling job. Waktu antre dan durasi job diukur dari respons enqueue, dengan resolusi sama dengan interval polling 1 s. Metrik job masuk ke tahap saat job diajukan, sehingga job yang diajukan pada tahap 6x tetap tercatat di 6x walaupun baru selesai pada tahap berikutnya.

| Tahap | VU API / job | API p50 (ms) | API p95 (ms) | Request/s | Error rate | Queue wait p50 / p95 (ms) | Durasi job p50 / p95 (ms) | Job selesai | Job/s |
|---|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Normal (`baseline`) | 10 / 2 | 4,4 | 22,3 | 24,7 | 0% | 1.023 / 1.042 | 1.031 / 2.047 | 41 | 0,68 |
| 3x (`stress3`) | 30 / 6 | 6,3 | 70,6 | 82,2 | 0% | 1.028 / 3.683 | 2.026 / 4.072 | 109 | 1,82 |
| 6x (`stress6`) | 60 / 12 | 21,9 | 174,7 | 158,1 | 0% | 4.136 / 6.948 | 4.544 / 7.829 | 114 | 1,90 |
| Turun (`recovery`) | 10 / 2 | 29,6 | 393,4 | 110,2 | 0% | 4.372 / 7.796 | 5.136 / 8.776 | 85 | 1,42 |
| Selesai (`rampdown`) | 0 / 0 | 12,7 | 347,3 | 22,7 | 0% | 1.027 / 2.277 | 1.637 / 3.278 | 23 | 0,77 |

#### Analisis

- **Normal.** API sangat ringan (p95 22 ms). Job langsung diambil worker: waktu antre ≈ 1 s, yang sama dengan resolusi polling. Durasi job p50 sekitar 1 s, berisi UPDATE ±100.000 baris (`id % 20`) dan pencatatan history.
- **3x.** Latency API naik, tetapi p95 masih 71 ms. Throughput job naik hampir tiga kali lipat (0,68 → 1,82 job/s) karena job pada dua database berbeda berjalan paralel. p95 waktu antre mulai naik ke 3,7 s, karena job pada database yang sama dijalankan serial oleh lock eksekusi.
- **6x.** Throughput job hanya naik sedikit (1,82 → 1,90 job/s). Worker dan dua lock database sudah menjadi batas kapasitas, sehingga beban tambahan berubah menjadi antrean: p50 waktu antre naik ke 4,1 s dan p95 ke 6,9 s. API tetap responsif (p95 175 ms, tanpa error) karena SQL berat dijalankan worker di luar request HTTP.
- **Turun.** p95 API justru tertinggi pada tahap ini (393 ms, masih di bawah threshold 500 ms), dan waktu antre p95 masih 7,8 s. Penyebabnya adalah antrean dari tahap 6x yang masih diproses. Worker terus menjalankan UPDATE besar pada database yang sama dengan query history dan daftar job yang dibaca API, sementara VU job lama masih mem-polling status.
- **Selesai.** Setelah antrean habis, waktu antre p50 kembali ke ≈ 1 s dan throughput turun mengikuti beban. Sistem pulih tanpa error dan tanpa job gagal.
- **Batas utama** ada pada eksekusi migration, bukan API. Satu worker dengan satu lock per database membatasi throughput sekitar 1,9 job/s untuk dua database. Menambah worker hanya membantu jika job tersebar ke lebih banyak database. Job pada database yang sama memang sengaja dijalankan serial.

Angka di atas bergantung pada kapasitas host dan beban database lokal. Pengukuran di mesin lain dapat memberi hasil berbeda.

Script mencatat latency, error rate, throughput request, waktu antre, durasi job, dan throughput penyelesaian job menurut fase beban. Keberhasilan job dihitung setelah status terminal berhasil.
