# IF3110 Milestone 1 — Database Management Platform

Fondasi aplikasi web pengelolaan database untuk Tugas Besar IF3110. Backend menggunakan Go, sedangkan halaman dirender di server. **Aplikasi ini tidak menggunakan SPA.**

> Status saat ini masih berupa scaffold: server, halaman awal, aset CSS, dan satu test handler sudah tersedia. Autentikasi, authorization, koneksi PostgreSQL, database viewer, serta fitur migration belum diimplementasikan.

## Tech stack

- **Go 1.23 atau lebih baru** untuk backend.
- **Templ** untuk membuat halaman HTML server-rendered.
- **`net/http` dan `ServeMux`** dari standard library untuk HTTP server dan routing.
- **PostgreSQL** sebagai DBMS target. `pgx/v5` direncanakan untuk koneksi database, tetapi belum terhubung pada scaffold ini.
- **CSS biasa** untuk aset frontend; tidak ada frontend framework atau client-side router.
- **`testing` dan `httptest`** dari Go untuk automated test.

## Struktur proyek

```text
cmd/web/             Entry point server
internal/web/        Router, handler, dan halaman awal
internal/auth/       Authentication dan session (placeholder)
internal/config/     Konfigurasi aplikasi (placeholder)
internal/database/   Koneksi PostgreSQL dan database viewer (placeholder)
internal/migration/  Editor, eksekusi, dan riwayat migration (placeholder)
internal/platform/   User, group, dan administrasi platform (placeholder)
db/migrations/       Schema database milik platform (placeholder)
web/static/          CSS dan aset statis
extra/               Dokumen pendukung lokal; seluruh folder diabaikan Git
```

Folder placeholder belum berisi implementasi. Pembagian tanggung jawab yang diusulkan untuk Aufar, Emil, dan Kevin ada di `extra/Pembagian-Kerja-Milestone-1.md` pada komputer lokal.

## Prasyarat

1. Pasang Go versi **1.23 atau lebih baru**.
2. Jalankan perintah dari direktori root repository.
3. Akses internet diperlukan saat pertama kali mengunduh dependency Go dan generator Templ.
4. `make` opsional, hanya diperlukan jika ingin memakai target Makefile.
5. PostgreSQL belum diperlukan untuk menjalankan scaffold saat ini. Database baru diperlukan setelah modul database terhubung.

## Menyiapkan proyek

Unduh dependency yang tercatat di `go.mod`:

```sh
go mod download
```

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
  │ GET /
  ▼
cmd/web/main.go
  │ Membaca PORT (default 8080) dan memulai http.ListenAndServe
  ▼
internal/web.New() → net/http ServeMux
  ├─ GET /         → home() dari home.templ
  │                   → home_templ.go yang dihasilkan Templ
  │                   → HTML dikirim ke browser
  └─ GET /static/* → file dari web/static/
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

## Test dan pemeriksaan

Jalankan dari root proyek:

```sh
go test ./...       # Jalankan unit dan integration test yang tersedia
go vet ./...        # Periksa masalah umum pada kode Go
go build ./...      # Pastikan seluruh paket dapat dikompilasi
```

Jika mengubah file `.templ`, jalankan generator lebih dahulu atau gunakan `make test`. Test handler HTTP dapat memakai `net/http/httptest`; test database nantinya dapat memakai database khusus pengujian. Setiap fitur backend perlu menguji alur berhasil, kegagalan, dan penolakan akses sesuai kebutuhan fitur.

## Pengaturan saat ini

| Nama | Status | Keterangan |
|---|---|---|
| `PORT` | Digunakan | Port HTTP; default `8080`. |
| `DATABASE_URL` | Belum digunakan | Contoh konfigurasi untuk pekerjaan database berikutnya, bukan koneksi aktif. |

Aplikasi saat ini tidak membaca file `.env`. Atur environment variable di shell atau lingkungan proses ketika fitur yang memerlukannya sudah diimplementasikan.

## Kendala umum

- **Port sudah digunakan:** pilih port lain melalui `PORT`.
- **Halaman Templ tidak sesuai perubahan terakhir:** jalankan `go run github.com/a-h/templ/cmd/templ@v0.3.960 generate`.
- **Error `malformed go.sum`:** periksa perubahan `go.sum`, lalu jalankan `go mod tidy`; jangan menambahkan baris checksum secara manual.
- **CSS tidak ditemukan:** pastikan server dijalankan dari root repository agar path `web/static` ditemukan.
- **Koneksi database gagal/belum tersedia:** scaffold saat ini belum membuka koneksi PostgreSQL; `DATABASE_URL` belum dibaca oleh program.

## Alur kontribusi yang disarankan

1. Sepakati interface lintas modul dan keputusan akses sebelum membagi implementasi paralel.
2. Kerjakan fitur dalam branch terpisah dan batasi perubahan pada folder modul yang menjadi tanggung jawab masing-masing.
3. Jika perlu mengubah kontrak bersama, `go.mod`, router utama, atau layout bersama, koordinasikan terlebih dahulu.
4. Jalankan `gofmt` pada berkas Go yang berubah, generate ulang Templ bila diperlukan, lalu jalankan `go test ./...`, `go vet ./...`, dan `go build ./...`.
5. Pada pull request, jelaskan skenario berhasil, gagal, dan akses ditolak yang sudah diuji.
