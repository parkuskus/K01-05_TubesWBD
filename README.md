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
internal/platform/   Backend administrasi user, group, dan keanggotaan
db/migrations/       Schema user, session, group, dan keanggotaan database platform
web/static/          CSS dan aset statis
extra/               Dokumen pendukung lokal; seluruh folder diabaikan Git
```

Folder `internal/database` dan `internal/migration` masih placeholder. Pembagian tanggung jawab yang diusulkan untuk Aufar, Emil, dan Kevin ada di `extra/Pembagian-Kerja-Milestone-1.md` pada komputer lokal.

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

`platform.Service` mengimplementasikan `contracts.Authorizer`: hak admin platform dibaca dari user terkini, sedangkan izin database dihitung dari grant seluruh group pengguna setiap kali diperiksa. Query izin database belum dapat dijalankan sebelum migration grant `0005` tersedia. Modul pemakai meminta pool target melalui `DatabaseProvider.Pool(ctx, databaseID)`. Provider bertanggung jawab mencari metadata koneksi berdasarkan ID dan mengelola pool; DSN serta password tidak dikembalikan kepada modul pemakai. Metadata koneksi dikelola di storage platform. Provider koneksi belum tersedia.

Riwayat migration disimpan **di setiap database target**, bukan di database platform, dalam schema khusus `if3110_platform` dengan tabel `migration_history` (`if3110_platform.migration_history`). Schema tersebut dicadangkan untuk metadata aplikasi; implementasi dan tabel dimiliki modul migration.

**Kontrak UI/route yang disepakati:** setiap modul memiliki handler dan template Templ sendiri; `ui.Layout(title string, body templ.Component)` di `internal/ui` menyediakan kerangka HTML bersama (judul dokumen dan isi dalam `<main>`). Layout tidak membuat navigasi atau pemeriksaan izin: handler tetap wajib memanggil `contracts.Authorizer` di backend. `internal/web` menjadi satu-satunya pemilik komposisi middleware dan router untuk memasang handler modul pada `/platform/`, `/databases/`, dan `/migrations/`. Modul tidak mengimpor `internal/web`; satu integrator mengubah `internal/web/server.go` agar tidak terjadi konflik. Route/UI modul belum tersedia dan halaman login/home yang sudah ada belum dipindahkan ke layout baru.

**Status keputusan:** aturan agregasi grant dan pemisahan hak Platform Admin telah disetujui tim. Batas provider koneksi, lokasi riwayat migration, dan batas UI/route adalah keputusan yang disetujui Aufar dan menunggu tinjauan Emil serta Kevin. Layout `internal/ui` sudah tersedia; provider koneksi dan UI modul belum diimplementasikan. Kebijakan session yang digunakan adalah idle 30 menit, absolute 12 jam, rotasi 15 menit, dan grace period 30 detik.

**Handoff Kevin/Emil:** gunakan `platform.NewService(db)` untuk `ListUsers/CreateUser/UpdateUsername/SetPassword/SetPlatformAdmin/DeleteUser`, `ListGroups/CreateGroup/RenameGroup/DeleteGroup/ListMembers/SetMembership`, serta `ListGrants/SetGrant`. Untuk pemeriksaan per request gunakan `contracts.Authorizer.RequirePlatformAdmin(ctx, userID)` dan `RequireDatabaseAccess(ctx, userID, databaseID, action)` (`platform.Service` mengimplementasikannya). Render halaman modul melalui `ui.Layout(title, body)`. Kontrak pool target untuk Emil tetap `contracts.DatabaseProvider.Pool(ctx, databaseID) (*sql.DB, error)`; kredensial/DSN tidak diteruskan ke modul pemakai. Grant dan pemeriksaan izin database belum dapat dipakai di PostgreSQL hingga migration `0004`/`0005` tersedia.

**Usulan handoff administrasi platform (perlu tinjauan Kevin/Emil):** `internal/platform` menyediakan model `User` (ID, username, status admin), `Group` (ID, nama), dan `Grant` (ID group, ID database, level); tidak ada hash password atau kredensial koneksi di model publik. `platform.NewService(db)` menyediakan operasi daftar/buat/ubah/hapus user dan group, `ListMembers`, serta `SetMembership` untuk menambah atau menghapus keanggotaan secara idempoten. `ListGrants(ctx, actorID, groupID)` dan `SetGrant(ctx, actorID, groupID, databaseID, level)` sudah memiliki validasi dan tes unit; `AccessNone` menghapus grant. `RequirePlatformAdmin` dan `RequireDatabaseAccess` tersedia pada service; yang pertama telah diuji dengan PostgreSQL, yang kedua baru dengan fake. **Operasi grant dan izin database PostgreSQL belum siap digunakan:** tabel `group_database_access` dan FK-nya menunggu migration koneksi `0004` Emil, kemudian migration `0005` dan tes PostgreSQL harus dibuat sebelum digunakan. Tiap metode administrasi menerima `ctx` dan ID aktor terautentikasi; repository memeriksa status Platform Admin di backend. Error domain: `ErrInvalidInput`, `ErrNotFound`, `ErrConflict`, dan `contracts.ErrForbidden`. ID database pada grant nantinya merujuk ID koneksi terdaftar milik Emil. Schema platform diurutkan: `0003_create_groups.sql` (Aufar), migration koneksi (Emil), lalu migration grant (Aufar). Login memeriksa ulang hash password dan status admin melalui `LoginProof` sambil mengunci user dalam transaksi pembuatan session; bukti lama ditolak tanpa menerbitkan cookie. Tes PostgreSQL memerlukan `PLATFORM_TEST_DATABASE_URL` dalam environment proses agar tidak dilewati.

## Test dan pemeriksaan

### Menyiapkan PostgreSQL khusus test

Test integrasi **membuat dan menghapus schema sementara**. Jangan arahkan `PLATFORM_TEST_DATABASE_URL` ke database development, production, atau database target pengguna. Gunakan database tersendiri bernama `platform_test`; test platform menolak nama database selain itu. Jika ada beberapa instalasi PostgreSQL (misalnya Windows dan Docker), pilih host dan port instalasi yang sama untuk pembuatan database dan URL test.

1. Masuk ke PostgreSQL lokal dengan akun administrator (contoh Windows dengan `psql` di `PATH`):

   ```powershell
   psql -h localhost -p 5432 -U postgres -d postgres -W
   ```

   `-W` meminta password secara interaktif. Di dalam `psql`, cek apakah role dan database sudah ada sebelum membuatnya:

   ```sql
   \du platform_test_user
   SELECT datname FROM pg_database WHERE datname = 'platform_test';
   ```

   Jika role **belum ada**, jalankan `CREATE ROLE` dan tetapkan password dengan `\password`. Jika database **belum ada**, jalankan `CREATE DATABASE` secara terpisah; jangan ulangi perintah yang objeknya sudah ada:

   ```sql
   CREATE ROLE platform_test_user LOGIN;
   \password platform_test_user
   CREATE DATABASE platform_test OWNER platform_test_user;
   ```

   `\password` meminta password baru tanpa menampilkannya dalam riwayat SQL. Keluar dengan `\q`. Jika database sudah ada dan memang khusus test tetapi bukan milik role ini, admin dapat memberikan izin `GRANT CONNECT, CREATE ON DATABASE platform_test TO platform_test_user;` tanpa menghapus database tersebut.

2. Uji koneksi role test; jangan melanjutkan jika autentikasi atau nama database masih salah:

   ```powershell
   psql -h localhost -p 5432 -U platform_test_user -d platform_test -W
   ```

   Setelah berhasil masuk, keluar dengan `\q`. Isi `PLATFORM_TEST_DATABASE_URL` dalam `.env` **lokal** dengan bentuk `postgres://platform_test_user:<PASSWORD>@localhost:5432/platform_test?sslmode=disable`. Ganti `<PASSWORD>` dengan password role test; karakter khusus di password harus di-*URL-encode*. Jangan commit `.env`, menyalin URL ke laporan bug, atau memakai kredensial admin PostgreSQL untuk test.

3. Test platform membaca URL **hanya dari environment proses**, bukan dari `.env`. Di PowerShell, muat nilainya tanpa mencetak URL (jalankan ulang setelah mengubah `.env`, karena environment proses yang lama tidak berubah otomatis):

   ```powershell
   $line = Get-Content .env | Where-Object { $_ -match '^\s*(?:export\s+)?PLATFORM_TEST_DATABASE_URL\s*=' } | Select-Object -Last 1
   if (-not $line) { throw 'PLATFORM_TEST_DATABASE_URL tidak ditemukan di .env' }
   $env:PLATFORM_TEST_DATABASE_URL = ($line -replace '^\s*(?:export\s+)?PLATFORM_TEST_DATABASE_URL\s*=\s*', '').Trim().Trim('"', "'")
   $uri = [uri]$env:PLATFORM_TEST_DATABASE_URL
   if ($uri.Scheme -notin @('postgres', 'postgresql') -or $uri.AbsolutePath -ne '/platform_test') { throw 'URL test harus menunjuk ke platform_test' }
   ```

4. Dari root proyek, jalankan test PostgreSQL khusus race login, kemudian seluruh suite dan pemeriksaan Go:

   ```powershell
   go test ./internal/platform -run '^TestLoginProofRejectsSessionAfterPasswordOrRoleChange$' -count=1 -v
   go test ./... -count=1
   go vet ./...
   go build ./...
   Remove-Item Env:PLATFORM_TEST_DATABASE_URL
   ```

   Pastikan hasil test integrasi **PASS**, bukan **SKIP**. Setiap test PostgreSQL membuat schema acak sendiri dan menghapusnya saat selesai. Test session lama dapat membaca `.env` sebagai fallback; test platform membutuhkan variabel proses seperti langkah 3. Tanpa koneksi test yang siap, unit test dapat dijalankan tanpa fallback lama dengan `go test ./... -count=1 -skip '^TestPostgresSessionStoreLifecycle$'`; test platform akan **SKIP** dan belum membuktikan perilaku PostgreSQL.

Jika mengubah file `.templ`, jalankan generator lebih dahulu atau gunakan `make test`. Test handler HTTP dapat memakai `net/http/httptest`. Setiap fitur backend perlu menguji alur berhasil, kegagalan, dan penolakan akses sesuai kebutuhan fitur.

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
- **`could not create isolated test schema`:** pastikan role/password pada URL test benar-benar dapat masuk ke `platform_test`, role memiliki izin `CREATE` pada database, dan variabel environment proses sudah dimuat ulang setelah `.env` diedit. Beberapa instalasi PostgreSQL di komputer yang sama dapat memakai port berbeda.
- **Startup meminta bootstrap admin:** isi `INITIAL_ADMIN_USERNAME` dan `INITIAL_ADMIN_PASSWORD` hanya saat database platform belum memiliki user.
- **CSRF key tidak valid:** atur `CSRF_AUTH_KEY` sebagai 64 karakter hex (32 byte acak).
- **Koneksi database gagal/belum tersedia:** pastikan PostgreSQL berjalan dan `PLATFORM_DATABASE_URL` menunjuk ke database platform yang benar.

## Alur kontribusi yang disarankan

1. Sepakati interface lintas modul dan keputusan akses sebelum membagi implementasi paralel.
2. Kerjakan fitur dalam branch terpisah dan batasi perubahan pada folder modul yang menjadi tanggung jawab masing-masing.
3. Jika perlu mengubah kontrak bersama, `go.mod`, router utama, atau layout bersama, koordinasikan terlebih dahulu.
4. Jalankan `gofmt` pada berkas Go yang berubah, generate ulang Templ bila diperlukan, lalu jalankan `go test ./...`, `go vet ./...`, dan `go build ./...`.
5. Pada pull request, jelaskan skenario berhasil, gagal, dan akses ditolak yang sudah diuji.
