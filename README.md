# IF3110 Database Management & Migration Platform

Aplikasi berbasis web untuk mengelola koneksi database PostgreSQL, melihat struktur schema dan isi tabel secara read-only, serta membuat dan menjalankan SQL migration dengan akses berbasis grup.

Dibuat untuk Tugas Besar Milestone 1 IF3110 Pengembangan Aplikasi Berbasis Web.

## Tech Stack

- **Backend**: Go 1.24 (`net/http`), PostgreSQL (`pgx/v5`)
- **Frontend**: Server-rendered HTML dengan [`a-h/templ`](https://templ.guide/) dan vanilla CSS
- **Security**: `bcrypt` untuk password hashing, AES-256-GCM untuk enkripsi kredensial database target, serta proteksi CSRF (`gorilla/csrf`)

## Fitur Utama

- **Autentikasi dan Manajemen Sesi**
  Login menggunakan username dan password dengan sesi berbasis PostgreSQL (idle timeout 30 menit, absolute timeout 12 jam, dan rotasi token berkala).
- **Administrasi Platform (Platform Admin)**
  Kelola akun pengguna, grup, anggota grup, serta pembagian hak akses grup ke setiap database target (`View Only` atau `View + Edit`). Perubahan yang mengurangi hak akses otomatis mencabut sesi aktif pengguna terdampak.
- **Manajemen Koneksi Database Target**
  Simpan, uji koneksi, ubah, dan hapus koneksi PostgreSQL target. Password database disimpan terenkripsi menggunakan AES-256-GCM.
- **Database Schema & Data Viewer**
  Jelajahi schema, daftar tabel, detail kolom, constraint, index, serta isi tabel secara read-only dengan pagination.
- **SQL Migration Management**
  Buat draft migration, tinjau SQL sebelum eksekusi, dan jalankan migration secara transaksional (`COMMIT` untuk `APPLIED`, `ROLLBACK` untuk `FAILED`) dengan urutan versi linear yang dicatat pada `if3110_platform.migration_history`.
- **Schema Diff & Auto-Draft Migration (Bonus)**
  Bandingkan dua schema PostgreSQL dan hasilkan draft SQL migration secara otomatis dari perbedaannya.

## Struktur Direktori

```text
cmd/
  migrate/           CLI migrasi schema database platform (versioned + SHA-256 checksum ledger)
  seedbench/         Generator dataset deterministik (COPY protocol) untuk benchmark >1 detik
  web/               Entrypoint server HTTP
  worker/            Proses background worker untuk antrean job durable & eksekusi batch
internal/
  app/               Inisialisasi aplikasi dan routing utama
  approval/          Operation request, full-group leader approval, dan revalidasi pre-eksekusi
  auth/              Autentikasi, sesi PostgreSQL, dan middleware CSRF
  config/            Pembacaan konfigurasi environment dan .env
  contracts/         Kontrak lintas komponen (ExecutionPlan, JobQueue, CommitEvidence, dll.)
  database/          Koneksi database target, enkripsi kredensial, viewer, dan schema diff
  definition/        Penyimpanan definisi/revisi migration M2 dan static validator
  execution/         Worker runtime, session advisory lock, ordinary/batch executor, dan recovery
  jobs/              Antrean job durable PostgreSQL, lease heartbeat, retry, dan progress reader
  migration/         Penyimpanan draft, eksekusi transaksional, dan riwayat migration
  platform/          Logika domain user, grup, dan hak akses (RBAC)
  platformdb/        Koneksi database platform dan skrip migrasi internal
  platformui/        Handler dan template UI untuk halaman admin platform
  telemetry/         Instrumentasi structured logging (slog) dan OpenTelemetry tracing
  web/               Handler dan template halaman beranda dan login
web/static/          File CSS statis
```

## Prasyarat

- Go 1.24+
- PostgreSQL (lokal atau container)

## Cara Menjalankan

1. **Salin file konfigurasi `.env`**

   ```bash
   cp .env.example .env
   ```

2. **Siapkan database PostgreSQL**

   Buat database untuk platform (misalnya `platform_dev`), lalu sesuaikan nilai `PLATFORM_DATABASE_URL` di `.env`:

   ```env
   PORT=8080
   APP_ENV=development
   PLATFORM_DATABASE_URL=postgres://postgres:password@localhost:5432/platform_dev?sslmode=disable
   SECURE_COOKIES=false
   CSRF_AUTH_KEY=<64-karakter-hex>
   DATABASE_CREDENTIAL_KEY=<64-karakter-hex>
   INITIAL_ADMIN_USERNAME=admin
   INITIAL_ADMIN_PASSWORD=admin12345
   ```

   Untuk membuat kunci hex 32-byte (`CSRF_AUTH_KEY` dan `DATABASE_CREDENTIAL_KEY`):

   ```bash
   openssl rand -hex 32
   ```

3. **Download dependency dan jalankan server**

   ```bash
   go mod download
   go run ./cmd/web
   ```

   Atau menggunakan `Makefile`:

   ```bash
   make run
   ```

   Saat pertama kali dijalankan, aplikasi otomatis menerapkan schema internal platform dan membuat akun admin awal dari `INITIAL_ADMIN_USERNAME` dan `INITIAL_ADMIN_PASSWORD` jika tabel pengguna masih kosong.

4. **Buka di browser**

   Akses `http://localhost:8080/login` dan masuk menggunakan akun admin yang telah dikonfigurasi.

## Variabel Environment

| Variabel | Wajib | Keterangan |
| :--- | :---: | :--- |
| `PLATFORM_DATABASE_URL` | Ya | Connection string PostgreSQL untuk metadata platform |
| `CSRF_AUTH_KEY` | Ya | Kunci 32-byte (hex 64 karakter / base64) untuk validasi CSRF |
| `DATABASE_CREDENTIAL_KEY` | Ya | Kunci 32-byte (hex 64 karakter / base64) untuk enkripsi password DB target |
| `PORT` | Tidak | Port HTTP server (default: `8080`) |
| `APP_ENV` | Tidak | Mode environment (`development` / `production`) |
| `SECURE_COOKIES` | Tidak | Set `true` jika berjalan di balik HTTPS (default: `false`) |
| `INITIAL_ADMIN_USERNAME` | Tidak | Username admin awal saat database masih kosong |
| `INITIAL_ADMIN_PASSWORD` | Tidak | Password admin awal (minimal 8 karakter, wajib bersama username) |
| `WORKER_ID` | Tidak | Identitas unik instance worker (`<hostname>-<pid>` jika kosong) |
| `WORKER_CONCURRENCY` | Tidak | Jumlah slot eksekusi paralel lintas database target berbeda (default: `4`) |
| `WORKER_LEASE_DURATION` | Tidak | Durasi lease klaim job di tabel `jobs` (default: `30s`) |
| `WORKER_HEARTBEAT_INTERVAL` | Tidak | Interval heartbeat perpanjangan lease worker (default: `10s`) |
| `WORKER_POLL_INTERVAL` | Tidak | Interval polling klaim job baru ketika antrean kosong (default: `2s`) |

## Daftar Route Utama

| Route | Akses | Fungsi |
| :--- | :--- | :--- |
| `GET /`, `GET /login`, `POST /login`, `POST /logout` | Publik / Login | Beranda workspace, halaman login, dan akhir sesi |
| `GET /admin/users`, `POST /admin/users/*` | Platform Admin | Kelola akun pengguna, password, dan status admin |
| `GET /admin/groups`, `GET /admin/groups/{id}` | Platform Admin | Kelola grup, anggota grup, dan hak akses database |
| `GET /databases`, `GET /databases/{id}/edit` | Platform Admin | Daftar koneksi database target, tambah, uji, dan edit koneksi |
| `GET /databases/{id}` | `View Only` / `View + Edit` | Lihat schema, struktur tabel, dan isi data (read-only) |
| `GET /databases/{id}/schemas/diff` | `View Only` / `View + Edit` | Bandingkan dua schema dan buat draft migration (membutuhkan `View + Edit` untuk membuat draft) |
| `GET /databases/{id}/migrations` | `View Only` / `View + Edit` | Riwayat dan detail versi SQL migration |
| `GET /databases/{id}/migrations/new` | `View + Edit` | Buat, edit, tinjau, dan jalankan SQL migration |

> **Catatan akses**: Platform Admin mengelola pengguna, grup, dan daftar koneksi database, tetapi tidak otomatis bisa melihat isi database target kecuali akunnya tergabung ke dalam grup yang memiliki akses ke database tersebut. Jika seorang pengguna tergabung di beberapa grup, hak akses tertinggi (`View + Edit` > `View Only`) yang berlaku.

### API JSON database (M2-A03)

Semua route berikut memerlukan sesi; perubahan data memerlukan header `X-CSRF-Token`. Respons memakai envelope `data` atau `error`, dan ID dikirim sebagai string.

| Route `/api/v1` | Akses | Fungsi |
| :--- | :--- | :--- |
| `GET/POST /connections`, `GET/PATCH/DELETE /connections/{id}` | Platform Admin | Kelola metadata koneksi; password hanya diterima pada request, tidak dikembalikan. `PATCH` mengirim seluruh metadata dan boleh menghilangkan password untuk mempertahankan password lama. |
| `POST /connections/{id}/test` | Platform Admin | Ping koneksi tersimpan tanpa menjalankan SQL migration. |
| `GET /databases`, `GET /databases/{id}` | Pengguna dengan grant | Daftar dan detail nama target beserta level akses, tanpa host atau kredensial. |
| `GET /databases/{id}/schemas`, `/tables?schema=...`, `/table?schema=...&table=...` | `View Only` / `View + Edit` | Jelajahi struktur target. |
| `GET /databases/{id}/data?schema=...&table=...&page=1` | `View Only` / `View + Edit` | Data read-only, maksimum 50 baris per halaman; null dan nilai terpotong diberi penanda eksplisit. |
| `GET /databases/{id}/schema-diff?reference_schema=...&target_schema=...` | `View Only` / `View + Edit` | Pratinjau perubahan dan SQL hasil perbandingan; tidak mengeksekusi SQL atau membuat draft. |

### Definisi dan validasi migration M2 (M2-A04/A05)

`GET/POST /api/v1/databases/{id}/migrations` dan `GET/PATCH /api/v1/databases/{id}/migrations/{migrationId}` menyimpan draft M2 di **database platform**, terpisah dari riwayat migration M1 di database target. Pembacaan memerlukan grant target; pembuatan dan revisi memerlukan `View + Edit`. Mutasi tetap memerlukan sesi dan CSRF. Body `POST` berisi `title`, `up_sql`, `down_sql`, `irreversible`, `reason`, `mode` (`NORMAL`/`BATCH`), `batch_plan` (objek JSON untuk BATCH), dan `notes`. Body `PATCH` menambahkan `expected_revision` untuk menolak edit yang stale. Setiap revisi SQL append-only; percobaan eksekusi akan mengunci definisinya secara permanen. Menyimpan draft BATCH **tidak** berarti SQL batch sudah tervalidasi atau dapat dijalankan; validasi dan approval adalah tahap berikutnya.

`POST /api/v1/databases/{id}/migrations/{migrationId}/validate` mengembalikan laporan validasi untuk revisi tersimpan saat ini (`valid`, `errors`, `warnings`, `compatibility_notes`, revision, checksum); laporan dengan temuan tetap berstatus HTTP 200 dan `valid: false`. Pemeriksaan bersifat statis dan tidak menjalankan SQL atau membuktikan kompatibilitas aplikasi; approver tetap harus menilai catatan kompatibilitas. `batch_plan` memakai field `table_schema`, `table_name`, `primary_key_column`, `scope_where`, dan `batch_size`; batch size yang diterima saat ini 1–10.000 dan SQL harus sesuai template UPDATE yang didukung executor.

### Operation request dan approval seluruh grup (M2-A06)

`POST/GET /api/v1/databases/{id}/operation-requests`, `GET /api/v1/operation-requests/{requestId}`, dan `POST /api/v1/operation-requests/{requestId}/approvals` memulai dan meninjau request tanpa menjalankan atau enqueue SQL. Submit body: `migration_id`, `revision`, `direction` (`APPLY`, `ROLLBACK`, `FORWARD`, `RESUME`, `COMPENSATE`), opsional `expected_head`. Laporan validasi harus lolos. Request membekukan detail migration, checksum, target connection revision, policy version, dan satu requirement untuk **setiap** grup target yang memiliki grant; grup tanpa leader membuat submit gagal. Tidak ada auto-approve, termasuk jika pengaju memimpin grup.

Vote body: `migration_revision`, `review_round`, `group_id`, `checksum`; actor hanya diambil dari session. Hanya leader saat ini yang masih menjadi anggota dan grupnya masih memiliki akses target yang dapat menyetujui. Setiap grup perlu vote tersendiri, termasuk beberapa grup dengan leader yang sama; leader view-only boleh menyetujui tanpa memperoleh hak eksekusi. Request `APPROVED` hanya setelah semua requirement disetujui. Perubahan policy, leader, grant, koneksi, revision, atau checksum menandai request `STALE`; ajukan request baru untuk round baru. Persetujuan bukan izin enqueue/execute; planner dan worker tetap harus revalidasi.

### Worker Runtime, Mode Batch, Recovery Runbook, dan Benchmark >1 Detik (`M2-K01`..`M2-K10`)

Eksekusi SQL terhadap database target dilakukan secara asinkron oleh proses worker terpisah (`cmd/worker`) melalui antrean durable PostgreSQL (`jobs` dan `job_attempts`):

```bash
# 1. Jalankan migrasi schema database platform secara eksplisit
go run ./cmd/migrate   # atau: make migrate

# 2. Jalankan background execution worker
go run ./cmd/worker    # atau: make worker
```

- **Isolasi & Konkurensi per Database Target (`LOCK-01` & `LOCK-02`)**: Klaim job di antrean menerapkan *per-database exclusion* (`FOR UPDATE SKIP LOCKED`) dan worker mengakuisisi *session-level advisory lock* (`pg_try_advisory_lock`) berdasarkan identitas fisik database target (`current_database()` + `inet_server_addr()` + `inet_server_port()`). Dua job pada database target yang sama selalu dieksekusi secara serial, sedangkan job pada database target berbeda berjalan paralel hingga batas `WORKER_CONCURRENCY`.
- **Mode Biasa (`NORMAL`) vs Mode Batch (`BATCH`)**:
  - **Mode Biasa**: Seluruh SQL dijalankan atomik dalam satu transaksi database target beserta pencatatan bukti commit pada `if3110_platform.schema_migrations` dan pembaruan *active head* (`if3110_platform.schema_state`).
  - **Mode Batch**: Membekukan himpunan primary key target di sisi server (`if3110_platform.batch_recovery_rows`) dan memproses perubahan per batch menggunakan *keyset pagination* (`row_id > last_pk ORDER BY row_id ASC LIMIT batch_size`). Setiap batch di-commit secara atomik bersama checkpoint-nya di `if3110_platform.batch_checkpoints`.
- **Status Job & Pemulihan Gate (`PARTIAL`, `PARTIALLY_REVERTED`, `UNKNOWN`)**:
  - Jika worker crash setelah commit di DB target tetapi sebelum memperbarui antrean, status `UNKNOWN` direkonsiliasi otomatis oleh `RecoveryExecutor` dari bukti commit di `if3110_platform.schema_migrations` / `batch_checkpoints` tanpa mengeksekusi ulang SQL.
  - Jika eksekusi batch berhenti di tengah jalan (`PARTIAL`), database target dipasangi *execution gate* yang memblokir migrasi baru sampai dipulihkan melalui `RESUME` (melanjutkan dari checkpoint terakhir), `COMPENSATE` (*reverse batch compensation* dari batch terakhir mundur ke batch pertama), atau `FORWARD`.
- **Benchmark Dataset >1 Detik (`PERF-01`)**:
  ```powershell
  # Menjalankan generator dataset deterministik (>=1.000.000 baris) & benchmark PERF-01
  ./scripts/run-benchmark.ps1
  ```
  Dokumentasi lengkap arsitektur worker, *recovery runbook*, tabel hasil pengukuran benchmark >1 detik (`Baseline 1..3` vs `Mode Batch` vs `Fail+Resume`), contoh JSON log & OpenTelemetry span, serta *failure drills* tersedia di [`docs/milestone-2/worker-and-benchmark-evidence.md`](docs/milestone-2/worker-and-benchmark-evidence.md).

## Pengujian (Testing)

Untuk menjalankan seluruh unit test:

```bash
go test ./...
```

Jika ingin mengikutsertakan integration test database PostgreSQL, isi variabel berikut pada `.env` atau environment shell sebelum menjalankan `go test ./...`:

- `PLATFORM_TEST_DATABASE_URL`
- `TARGET_TEST_DATABASE_URL`
- `MIGRATION_READONLY_TEST_DATABASE_URL` (opsional, untuk menguji penolakan koneksi read-only saat migrasi)

Jika mengubah file template `.templ`, jalankan generator sebelum build atau test:

```bash
go run github.com/a-h/templ/cmd/templ@v0.3.960 generate
```

## Kontributor

| Nama | NIM |
| :--- | :---: |
| Kevin Wirya Valerian | 13524019 |
| Emilio Justin | 13524043 |
| Muhammad Aufar Rizqi Kusuma | 13524061 |
