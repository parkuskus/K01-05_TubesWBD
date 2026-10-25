# IF3110 Milestone 1 — Database Management Platform

Fondasi aplikasi web pengelolaan database untuk Tugas Besar IF3110. Backend menggunakan Go, sedangkan halaman dirender di server. **Aplikasi ini tidak menggunakan SPA.**

> Status saat ini: login/logout, administrasi pengguna dan grup, grant, koneksi target, viewer, serta migration telah terpasang pada server. Tes integrasi PostgreSQL membutuhkan tiga database khusus uji; tes yang `SKIP` tidak membuktikan fitur tersebut.

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
internal/database/   Koneksi PostgreSQL dan database viewer
internal/migration/  Editor, eksekusi, dan riwayat migration target
internal/platformui/ UI administrasi pengguna, grup, dan grant
internal/platform/   Backend administrasi user, group, dan keanggotaan
db/migrations/       Schema user, session, group, dan keanggotaan database platform
web/static/          CSS dan aset statis
extra/               Dokumen pendukung lokal; seluruh folder diabaikan Git
```

Folder `extra/` berisi dokumen pendukung lokal dan diabaikan Git.

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

Salin `.env.example` menjadi `.env`, lalu atur `PLATFORM_DATABASE_URL`, `CSRF_AUTH_KEY`, dan `DATABASE_CREDENTIAL_KEY`. Kedua kunci harus masing-masing 64 karakter hex (32 byte) dan **berbeda**. `INITIAL_ADMIN_USERNAME` dan `INITIAL_ADMIN_PASSWORD` hanya dibutuhkan ketika tabel `users` masih kosong. File `.env` dibaca saat startup jika tersedia; variabel environment proses tetap memiliki prioritas.

Untuk setiap kunci, buat nilai acak tersendiri di PowerShell (jangan kirim atau commit hasilnya):

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

## Alur aplikasi dan route aktif

Saat startup, `cmd/web` memuat konfigurasi, menghubungi `PLATFORM_DATABASE_URL`, menerapkan migration platform, dan membuat admin pertama bila tabel pengguna masih kosong. Semua halaman kerja memakai sesi; POST memakai CSRF. Route utama:

Username peka terhadap huruf besar-kecil; spasi di awal/akhir dihapus. Akun yang sudah ada sebelum aturan ini berubah tersimpan dalam lowercase, sehingga username lama harus dimasukkan dalam lowercase. Password tetap peka huruf besar-kecil.

| Route | Fungsi |
|---|---|
| `GET/POST /login`, `POST /logout`, `GET /` | Masuk, keluar, beranda. |
| `/platform/users`, `/platform/groups`, `/platform/groups/{id}` | Kelola pengguna, grup, anggota, dan grant (Platform Admin). |
| `/databases/connections/`, `/new`, `/{id}/edit`, `/{id}/delete` | Kelola koneksi target (Platform Admin); penghapusan memakai halaman konfirmasi. |
| `/databases/`, `/{id}/schemas`, `/{id}/tables`, `/{id}/table`, `/{id}/data` | Pilih database sesuai grant, lihat struktur dan data; `table`/`data` memakai parameter `schema` dan `table`, `data` memakai `page`. |
| `/databases/{databaseID}/migrations`, `/new`, `/{version}` | Riwayat, editor, dan detail migration; POST membuat draft, mengubah draft, atau mengeksekusi setelah konfirmasi. |

`View Only` boleh membaca schema, data, dan riwayat; `View + Edit` juga boleh mengelola migration. Eksekusi SQL berhasil dicatat sebagai `APPLIED`; kegagalan melakukan rollback perubahan dalam transaksi dan mencatat `FAILED`. Pemeriksaan izin selalu dilakukan di backend; menyembunyikan tombol saja bukan kontrol akses. Tidak ada editor data baris per baris atau dukungan DBMS selain PostgreSQL.

Migration hanya dapat dijalankan bila **semua versi sebelumnya `APPLIED`**. Versi yang masih `DRAFT` atau berstatus `FAILED` menghalangi eksekusi berikutnya; saat ini belum ada alur untuk mengulang atau memperbaiki versi `FAILED`, sehingga jangan menjalankan migration gagal pada database yang perlu terus dimigrasikan tanpa rencana pemulihan.

## Model akses dan kontrak antarmodul

Package `internal/contracts/` menyediakan tipe `UserID`, `GroupID`, `DatabaseID`, `AccessLevel`, dan `DatabaseAction`, serta interface `Authorizer` dan `DatabaseProvider`. Kontrak ini memisahkan authorization dari koneksi database:

- Grant group untuk user dan database yang sama saat ini digabung dengan memilih tingkat tertinggi (`View + Edit` mengungguli `View Only`). Kebijakan ini sudah diimplementasikan, tetapi masih perlu dikonfirmasi bersama sebelum dianggap final.
- `View Only` mengizinkan melihat schema, data, dan riwayat migration.
- `View + Edit` juga mengizinkan membuat, mengubah, dan menjalankan migration.
- Grant atau aksi yang nilainya tidak dikenal ditolak (*fail closed*).
- Status Platform Admin terpisah dari akses database; admin tetap memerlukan grant group untuk melihat atau mengubah database target.

`platform.Service` mengimplementasikan `contracts.Authorizer`: hak admin dan grant dibaca ulang pada pemeriksaan berikutnya. Grant disimpan di database platform dengan FK ke koneksi target (`db/migrations/0005_*`). Modul viewer/migration meminta pool target berdasarkan ID melalui `DatabaseProvider.Pool`; password koneksi disimpan terenkripsi di platform dan tidak dikirim ke halaman.

Riwayat migration disimpan **di setiap database target**, bukan di database platform, dalam schema khusus `if3110_platform` dengan tabel `migration_history` (`if3110_platform.migration_history`). Schema tersebut dicadangkan untuk metadata aplikasi; implementasi dan tabel dimiliki modul migration.

`ui.Layout` menyediakan kerangka HTML bersama; `internal/web` memasang route dan middleware. Handler modul mempertahankan pemeriksaan izin sendiri. Keputusan **grant tertinggi menang** sudah diterapkan, tetapi masih perlu persetujuan akhir tim sebelum disebut kebijakan final. Kebijakan sesi: idle 30 menit, umur maksimal 12 jam, rotasi 15 menit, grace period 30 detik.

Perubahan keanggotaan/grant yang benar-benar mengubah akses dan penghapusan grup mencabut sesi anggota terdampak pada transaksi yang sama. Pengguna tersebut perlu login kembali; perubahan idempoten tidak mencabut sesi.

## Test dan pemeriksaan

### Menyiapkan PostgreSQL khusus test

Tes integrasi **membuat dan menghapus schema sementara**, termasuk `if3110_platform` pada target. Siapkan tiga database **disposable** terpisah: `platform_test`, `target_test`, dan `migration_readonly_test`. Jangan arahkan URL tes ke database pengembangan, produksi, atau database target pengguna; kedua database target tes migration harus belum memiliki schema `if3110_platform`. Jika ada beberapa instalasi PostgreSQL (misalnya Windows dan Docker), pilih host dan port yang sama saat membuat database dan menyusun URL.

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

   Setelah berhasil masuk, keluar dengan `\q`. Buat juga `target_test` dan `migration_readonly_test` sebagai database khusus uji dengan role yang memiliki izin `CONNECT` dan `CREATE`. Jangan memakai kredensial admin PostgreSQL untuk tes.

3. Atur ketiga URL **di environment proses** sebelum menjalankan tes. Sebagian tes mengabaikan `.env`, sedangkan tes sesi lama memakai `.env` sebagai fallback; nilai proses mencegah ketidakkonsistenan. Contoh PowerShell ini tidak mencetak kata sandi maupun URL:

   ```powershell
   $secret = Read-Host 'Password role PostgreSQL untuk tes' -AsSecureString
   $cred = [pscredential]::new('platform_test_user', $secret)
   $pw = [uri]::EscapeDataString($cred.GetNetworkCredential().Password)

   try {
       $env:PLATFORM_TEST_DATABASE_URL = "postgres://platform_test_user:${pw}@localhost:5432/platform_test?sslmode=disable"
       $env:TARGET_TEST_DATABASE_URL = "postgres://platform_test_user:${pw}@localhost:5432/target_test?sslmode=disable"
       $env:MIGRATION_READONLY_TEST_DATABASE_URL = "postgres://platform_test_user:${pw}@localhost:5432/migration_readonly_test?sslmode=disable"

       go test ./... -count=1 -v
       if ($LASTEXITCODE -ne 0) { throw 'Tes gagal; periksa FAIL dan SKIP tanpa membagikan URL.' }
       go vet ./...
       if ($LASTEXITCODE -ne 0) { throw 'go vet gagal.' }
       go build ./...
       if ($LASTEXITCODE -ne 0) { throw 'go build gagal.' }
   } finally {
       Remove-Item Env:PLATFORM_TEST_DATABASE_URL, Env:TARGET_TEST_DATABASE_URL, Env:MIGRATION_READONLY_TEST_DATABASE_URL -ErrorAction SilentlyContinue
       Remove-Variable pw, cred, secret -ErrorAction SilentlyContinue
   }
   ```

   Ganti nama role/host/port sesuai database uji milikmu; bila kata sandi memakai karakter khusus, `EscapeDataString` sudah meng-*URL-encode*-nya. Pastikan tes PostgreSQL **PASS**, bukan **SKIP**, terutama `TestPostgresEndToEndWebFlow` dan `TestPostgresMigrationServiceCommitAndRollback`. Jangan membagikan URL lengkap atau isi `.env`. Tanpa URL uji, hanya jalankan tes non-integrasi secara terpisah; hasil tersebut tidak membuktikan PostgreSQL.

Jika mengubah file `.templ`, jalankan generator lebih dahulu atau gunakan `make test`. Test handler HTTP dapat memakai `net/http/httptest`. Setiap fitur backend perlu menguji alur berhasil, kegagalan, dan penolakan akses sesuai kebutuhan fitur.

## Pengaturan saat ini

| Nama | Status | Keterangan |
|---|---|---|
| `PORT` | Digunakan | Port HTTP; default `8080`. |
| `DATABASE_URL` | Tidak dipakai aplikasi | Koneksi target dikelola melalui halaman admin; bukan storage platform. |
| `PLATFORM_DATABASE_URL` | Digunakan | Koneksi PostgreSQL platform untuk akun, session, dan migration internal. |
| `CSRF_AUTH_KEY` | Digunakan | Secret hex 64 karakter (32 byte) untuk proteksi CSRF; jangan commit. |
| `DATABASE_CREDENTIAL_KEY` | Wajib | Kunci hex 64 karakter (32 byte) untuk enkripsi kredensial koneksi; harus berbeda dari `CSRF_AUTH_KEY`. |
| `INITIAL_ADMIN_USERNAME` | Bootstrap awal | Username admin pertama; hanya diperlukan jika tabel `users` kosong. |
| `INITIAL_ADMIN_PASSWORD` | Bootstrap awal | Password admin pertama; hanya diperlukan jika tabel `users` kosong dan dihapus dari `.env` setelah bootstrap. |
| `APP_ENV` | Opsional | `development` secara default; gunakan `production` pada deployment. |
| `SECURE_COOKIES` | Opsional | `false` secara default untuk HTTP lokal; wajib `true` ketika `APP_ENV=production`. |
| `PLATFORM_TEST_DATABASE_URL` | Test integrasi opsional | Wajib menunjuk database PostgreSQL khusus pengujian; test membuat dan menghapus schema terisolasi. |
| `TARGET_TEST_DATABASE_URL` | Test integrasi opsional | Database target disposable untuk viewer, migration, dan tes HTTP gabungan. |
| `MIGRATION_READONLY_TEST_DATABASE_URL` | Test integrasi opsional | Database disposable untuk tes riwayat baca-saja dan kegagalan draft. |

Aplikasi membaca `.env` jika file tersedia, membuka koneksi dari `PLATFORM_DATABASE_URL`, lalu menerapkan migration platform secara otomatis saat startup. Jangan memakai koneksi database target pengguna untuk menyimpan akun atau session platform. Jangan membagikan isi `.env` atau memasukkan kredensial ke Git.

Session memakai cookie opaque `session_id`; token hanya disimpan sebagai hash di PostgreSQL. Nilai idle 30 menit, absolute 12 jam, rotasi 15 menit, dan grace period 30 detik adalah kebijakan yang digunakan. Cookie produksi menggunakan `Secure`; semua request yang mengubah state, termasuk login/logout, dilindungi CSRF.

## Kendala umum

- **Port sudah digunakan:** pilih port lain melalui `PORT`.
- **Halaman Templ tidak sesuai perubahan terakhir:** jalankan `go run github.com/a-h/templ/cmd/templ@v0.3.960 generate`.
- **Error `malformed go.sum`:** periksa perubahan `go.sum`, lalu jalankan `go mod tidy`; jangan menambahkan baris checksum secara manual.
- **CSS tidak ditemukan:** pastikan server dijalankan dari root repository agar path `web/static` ditemukan.
- **Tes PostgreSQL `SKIP`:** atur ketiga URL database uji di environment proses. Jangan arahkan ke database development atau production.
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
