# IF3110 Database Management & Migration Platform

Aplikasi berbasis web untuk mengelola koneksi database PostgreSQL, melihat struktur schema dan isi tabel secara read-only, serta membuat dan menjalankan SQL migration dengan akses berbasis grup.

Dibuat untuk Tugas Besar 2 IF3110 Pengembangan Aplikasi Berbasis Web.

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
cmd/web/             Entrypoint server HTTP
internal/
  app/               Inisialisasi aplikasi dan routing utama
  auth/              Autentikasi, sesi PostgreSQL, dan middleware CSRF
  config/            Pembacaan konfigurasi environment dan .env
  database/          Koneksi database target, enkripsi kredensial, viewer, dan schema diff
  migration/         Penyimpanan draft, eksekusi transaksional, dan riwayat migration
  platform/          Logika domain user, grup, dan hak akses (RBAC)
  platformdb/        Koneksi database platform dan skrip migrasi internal
  platformui/        Handler dan template UI untuk halaman admin platform
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
