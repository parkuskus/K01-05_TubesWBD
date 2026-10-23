# IF3110 Milestone 1 — Database Management Platform

Fondasi aplikasi web pengelolaan database untuk Tugas Besar IF3110. Backend menggunakan Go, sedangkan halaman dirender di server. **Aplikasi ini tidak menggunakan SPA.**

> Status saat ini: login/logout, bootstrap admin pertama, session PostgreSQL, dan CSRF sudah terhubung ke server. Authorization group, database viewer, koneksi database target, dan fitur migration belum diimplementasikan.

## Tech stack

- **Go 1.23 atau lebih baru** untuk backend.
- **Templ** untuk membuat halaman HTML server-rendered.
- **`net/http` dan `ServeMux`** dari standard library untuk HTTP server dan routing.
- **PostgreSQL** sebagai DBMS target dan storage session platform melalui `pgx/v5`.
- **bcrypt** untuk hashing password; **Gorilla CSRF** untuk melindungi form state-changing.
- **CSS biasa** untuk aset frontend; tidak ada frontend framework atau client-side router.
- **`testing` dan `httptest`** dari Go untuk automated test.

## Struktur proyek

```text
cmd/web/             Entry point server
internal/web/        Router, handler login/logout, dan halaman Templ
internal/contracts/  Tipe dan interface lintas modul
internal/auth/       Bootstrap admin, autentikasi, session PostgreSQL, dan CSRF
internal/config/     Pembacaan `.env` dan validasi konfigurasi
internal/database/   Koneksi PostgreSQL dan database viewer (placeholder)
internal/migration/  Editor, eksekusi, dan riwayat migration (placeholder)
internal/platform/   User, group, dan administrasi platform (placeholder)
db/migrations/       Schema user dan session database platform
web/static/          CSS dan aset statis
extra/               Dokumen pendukung lokal; seluruh folder diabaikan Git
```

Folder placeholder belum berisi implementasi. Pembagian tanggung jawab yang diusulkan untuk Aufar, Emil, dan Kevin ada di `extra/Pembagian-Kerja-Milestone-1.md` pada komputer lokal.

## Prasyarat

1. Pasang Go versi **1.23 atau lebih baru**.
2. Jalankan perintah dari direktori root repository.
3. Akses internet diperlukan saat pertama kali mengunduh dependency Go dan generator Templ.
4. `make` opsional, hanya diperlukan jika ingin memakai target Makefile.
5. PostgreSQL lokal diperlukan untuk menjalankan aplikasi setelah wiring auth aktif. Buat database platform terpisah, misalnya `platform_dev`; jangan pakai database test atau database target pengguna.

## Menyiapkan proyek

Unduh dependency yang tercatat di `go.mod`:

```sh
go mod download
```

Salin `.env.example` menjadi `.env`, lalu atur `PLATFORM_DATABASE_URL`, `CSRF_AUTH_KEY`, `INITIAL_ADMIN_USERNAME`, dan `INITIAL_ADMIN_PASSWORD`. File `.env` dibaca saat startup jika tersedia; variabel environment proses tetap memiliki prioritas. Password admin awal hanya dibutuhkan ketika tabel `users` masih kosong.

`CSRF_AUTH_KEY` harus berisi 64 karakter hex yang mewakili 32 byte acak. Buat lokal di PowerShell (jangan kirim atau commit hasilnya):

```powershell
$bytes = New-Object byte[] 32
$rng = [System.Security.Cryptography.RandomNumberGenerator]::Create()
$rng.GetBytes($bytes)
[BitConverter]::ToString($bytes).Replace('-', '').ToLowerInvariant()
$rng.Dispose()
```

Saat tabel user kosong, isi `INITIAL_ADMIN_USERNAME` dan `INITIAL_ADMIN_PASSWORD` dengan kredensial admin pertama. Setelah aplikasi berhasil start dan akun dibuat, hapus kedua nilai itu dari `.env`; akun tersimpan di PostgreSQL dan tidak dibuat ulang pada restart.

Berkas `.templ` harus digenerasikan menjadi berkas Go sebelum perubahan template dikompilasi. Proyek menyimpan hasil generate (`*_templ.go`) agar build biasa dapat dilakukan tanpa memasang binary Templ secara global.

```sh
go run github.com/a-h/templ/cmd/templ@v0.3.960 generate
```

Setelah mengubah dependency di `go.mod`, rapikan dan perbarui checksum:

```sh
go mod tidy
```

Jangan mengedit `go.sum` secara manual. Jangan commit berkas `.env` atau credential; `.env.example` hanya contoh nilai dan **tidak dimuat otomatis** oleh aplikasi.

## Menjalankan aplikasi

Dari root proyek:

```sh
go run ./cmd/web
```

Server mendengarkan di `http://localhost:8080` secara default. Untuk mengganti port, atur environment variable `PORT`.

**PowerShell:**

```powershell
$env:PORT = "8081"
go run ./cmd/web
```

Hapus nilai environment setelah selesai jika shell yang sama akan digunakan kembali:

```powershell
Remove-Item Env:PORT
```

**Bash atau Zsh:**

```sh
PORT=8081 go run ./cmd/web
```

Jika `make` tersedia, perintah berikut merupakan singkatan. `make run` akan menggenerasikan Templ lebih dahulu:

```sh
make generate  # Generate berkas Go dari file .templ
make run       # Generate lalu jalankan server
make test      # Generate lalu jalankan seluruh test
```

## Alur program saat ini

Diagram ini menunjukkan implementasi yang benar-benar tersedia sekarang:

```text
Browser
  │ GET /login
  ▼
cmd/web/main.go
  │ Membaca .env/environment, connect ke PLATFORM_DATABASE_URL,
  │ menerapkan migration platform, bootstrap admin jika belum ada
  ▼
internal/web.New() → CSRF middleware → ServeMux
  ├─ GET /login    → form username/password + CSRF token
  ├─ POST /login   → autentikasi, buat cookie session, redirect /
  ├─ GET /         → wajib punya session
  ├─ POST /logout  → wajib punya session + CSRF, cabut session
  └─ GET /static/* → aset dari web/static/
```

Untuk halaman baru, buat file `.templ` pada paket yang sesuai, jalankan generator Templ, lalu daftarkan handler melalui router. Perubahan file CSS atau aset statis disimpan di `web/static/`. Hindari SPA dan client-side router; navigasi dan pengiriman form dilakukan melalui request HTTP ke server.

## Alur aplikasi yang dituju

Alur berikut adalah target berdasarkan spesifikasi Milestone 1, **bukan fitur yang sudah aktif**:

```text
Browser
  → Login dan session
  → Authorization backend
  → Pilih database PostgreSQL
  → Lihat schema/data atau riwayat migration
  → (View + Edit) Susun dan tinjau SQL migration
  → Jalankan dalam transaction
      ├─ berhasil → COMMIT dan catat riwayat
      └─ gagal    → ROLLBACK dan catat status
```

Setiap request yang dilindungi harus diperiksa di backend. Menyembunyikan tombol di UI saja bukan kontrol akses.

## Model akses dan kontrak antarmodul

Package `internal/contracts/` menyediakan tipe `UserID`, `GroupID`, `DatabaseID`, `AccessLevel`, dan `DatabaseAction`, serta interface `Authorizer` dan `DatabaseProvider`. Kontrak ini memisahkan authorization dari koneksi database:

- Grant group untuk user dan database yang sama digabung dengan memilih tingkat tertinggi (`View + Edit` mengungguli `View Only`). Aturan ini telah disepakati tim.
- `View Only` mengizinkan melihat schema, data, dan riwayat migration.
- `View + Edit` juga mengizinkan membuat, mengubah, dan menjalankan migration.
- Grant atau aksi yang nilainya tidak dikenal ditolak (*fail closed*).
- Status Platform Admin terpisah dari akses database; admin tetap memerlukan grant group untuk melihat atau mengubah database target.

`Authorizer` memeriksa hak admin platform dan izin database secara terpisah. Modul pemakai meminta pool target melalui `DatabaseProvider.Pool(ctx, databaseID)`. Provider bertanggung jawab mencari metadata koneksi berdasarkan ID dan mengelola pool; DSN serta password tidak dikembalikan kepada modul pemakai. Metadata koneksi dikelola di storage platform. Implementasi repository authorization dan provider koneksi belum tersedia.

Riwayat migration disimpan **di setiap database target**, bukan di database platform, dalam schema khusus `if3110_platform` dengan tabel `migration_history` (`if3110_platform.migration_history`). Schema tersebut dicadangkan untuk metadata aplikasi; implementasi dan tabel dimiliki modul migration.

**Kontrak UI/route yang disepakati:** setiap modul memiliki handler dan template Templ sendiri; layout bersama berada di `internal/ui`; dan `internal/web` menjadi satu-satunya pemilik komposisi middleware dan router untuk memasang handler modul pada `/platform/`, `/databases/`, dan `/migrations/`. Modul tidak mengimpor `internal/web`; satu integrator mengubah `internal/web/server.go` agar tidak terjadi konflik. Batas ini mendokumentasikan rancangan, bukan klaim bahwa route atau UI modul telah tersedia.

**Status keputusan:** aturan agregasi grant dan pemisahan hak Platform Admin telah disetujui tim. Batas provider koneksi, lokasi riwayat migration, dan batas UI/route adalah keputusan yang disetujui Aufar dan menunggu tinjauan Emil serta Kevin. `internal/ui`, repository authorization, provider koneksi, dan modul fitur tersebut belum diimplementasikan. Kebijakan session yang digunakan adalah idle 30 menit, absolute 12 jam, rotasi 15 menit, dan grace period 30 detik.

**Usulan handoff administrasi platform (perlu tinjauan Kevin/Emil):** `internal/platform` mulai menyediakan model `User` (ID, username, status admin), `Group` (ID, nama), dan `Grant` (ID group, ID database, level); tidak ada hash password atau kredensial koneksi di model publik. Service administrasi yang akan dipakai UI Kevin direncanakan memiliki operasi daftar/buat/ubah/hapus user dan group, atur keanggotaan, serta daftar/atur grant; setiap operasi menerima ID aktor terautentikasi dan memeriksa status Platform Admin di backend. ID database pada grant harus merujuk ID koneksi terdaftar milik Emil. Perubahan schema platform diurutkan: `0003_create_groups.sql` (Aufar: group/keanggotaan), migration koneksi (Emil), lalu migration grant (Aufar, setelah nama tabel dan ID koneksi dikonfirmasi). Ini **belum** menyatakan service, tabel koneksi, atau tabel grant telah tersedia.

## Test dan pemeriksaan

Jalankan dari root proyek:

```sh
go test ./...       # Jalankan unit dan integration test yang tersedia
go vet ./...        # Periksa masalah umum pada kode Go
go build ./...      # Pastikan seluruh paket dapat dikompilasi
```

Jika mengubah file `.templ`, jalankan generator lebih dahulu atau gunakan `make test`. Test handler HTTP dapat memakai `net/http/httptest`; PostgreSQL session integration test memakai `PLATFORM_TEST_DATABASE_URL` dan dilewati jika tidak disetel. Setiap fitur backend perlu menguji alur berhasil, kegagalan, dan penolakan akses sesuai kebutuhan fitur.

## Pengaturan saat ini

| Nama | Status | Keterangan |
|---|---|---|
| `PORT` | Digunakan | Port HTTP; default `8080`. |
| `DATABASE_URL` | Belum digunakan | Koneksi database target yang nanti dikelola aplikasi; bukan storage session/user platform. |
| `PLATFORM_DATABASE_URL` | Digunakan | Koneksi PostgreSQL platform untuk akun, session, dan migration internal. |
| `CSRF_AUTH_KEY` | Digunakan | Secret hex 64 karakter (32 byte) untuk proteksi CSRF; jangan commit. |
| `INITIAL_ADMIN_USERNAME` | Bootstrap awal | Username admin pertama; hanya diperlukan jika tabel `users` kosong. |
| `INITIAL_ADMIN_PASSWORD` | Bootstrap awal | Password admin pertama; hanya diperlukan jika tabel `users` kosong dan dihapus dari `.env` setelah bootstrap. |
| `APP_ENV` | Opsional | `development` secara default; gunakan `production` pada deployment. |
| `SECURE_COOKIES` | Opsional | `false` secara default untuk HTTP lokal; wajib `true` ketika `APP_ENV=production`. |
| `PLATFORM_TEST_DATABASE_URL` | Test integrasi opsional | Wajib menunjuk database PostgreSQL khusus pengujian; test membuat dan menghapus schema terisolasi. |

Aplikasi membaca `.env` jika file tersedia, membuka koneksi dari `PLATFORM_DATABASE_URL`, lalu menerapkan migration internal secara otomatis saat startup. Jangan memakai koneksi database target pengguna untuk menyimpan akun atau session platform. `DATABASE_URL` tetap disediakan untuk koneksi database target pada fitur berikutnya. Jangan membagikan isi `.env` atau memasukkan kredensial ke Git.

Session memakai cookie opaque `session_id`; token hanya disimpan sebagai hash di PostgreSQL. Nilai idle 30 menit, absolute 12 jam, rotasi 15 menit, dan grace period 30 detik adalah kebijakan yang digunakan. Cookie produksi menggunakan `Secure`; semua request yang mengubah state, termasuk login/logout, dilindungi CSRF.

## Kendala umum

- **Port sudah digunakan:** pilih port lain melalui `PORT`.
- **Halaman Templ tidak sesuai perubahan terakhir:** jalankan `go run github.com/a-h/templ/cmd/templ@v0.3.960 generate`.
- **Error `malformed go.sum`:** periksa perubahan `go.sum`, lalu jalankan `go mod tidy`; jangan menambahkan baris checksum secara manual.
- **CSS tidak ditemukan:** pastikan server dijalankan dari root repository agar path `web/static` ditemukan.
- **Test PostgreSQL session dilewati:** atur `PLATFORM_TEST_DATABASE_URL` ke database pengujian khusus. Jangan arahkan ke database development atau production.
- **Startup meminta bootstrap admin:** isi `INITIAL_ADMIN_USERNAME` dan `INITIAL_ADMIN_PASSWORD` hanya saat database platform belum memiliki user.
- **CSRF key tidak valid:** atur `CSRF_AUTH_KEY` sebagai 64 karakter hex (32 byte acak).
- **Koneksi database gagal/belum tersedia:** pastikan PostgreSQL berjalan dan `PLATFORM_DATABASE_URL` menunjuk ke database platform yang benar.

## Alur kontribusi yang disarankan

1. Sepakati interface lintas modul dan keputusan akses sebelum membagi implementasi paralel.
2. Kerjakan fitur dalam branch terpisah dan batasi perubahan pada folder modul yang menjadi tanggung jawab masing-masing.
3. Jika perlu mengubah kontrak bersama, `go.mod`, router utama, atau layout bersama, koordinasikan terlebih dahulu.
4. Jalankan `gofmt` pada berkas Go yang berubah, generate ulang Templ bila diperlukan, lalu jalankan `go test ./...`, `go vet ./...`, dan `go build ./...`.
5. Pada pull request, jelaskan skenario berhasil, gagal, dan akses ditolak yang sudah diuji.
