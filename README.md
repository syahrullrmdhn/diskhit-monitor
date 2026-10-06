# DiskHist Monitor

Satu binary Go (`diskhist.exe`) yang menjalankan scanner storage dan web dashboard
dalam satu proses, sebagai Windows Service.

## Fitur

- **Exact Recursive Snapshots** — setiap scan menyimpan ukuran logis tiap folder/subfolder (termasuk seluruh turunannya) sampai `max_depth`
  - Nilai **Size** dihitung dari logical EOF bytes; **Size on disk** dihitung terpisah melalui `GetCompressedFileSizeW`
  - VSS `ClientAccessible` membuat pandangan point-in-time agar file aktif tidak berubah di tengah scan
  - Junction/reparse link tidak ditelusuri untuk mencegah loop dan double counting
  - VSS membutuhkan Windows Service berjalan dengan hak Administrator/LocalSystem dan volume yang mendukung VSS
  - Scan berstatus `partial` berarti ada path yang tidak dapat dibaca; hasil tidak boleh dianggap lengkap
- **Analisis Periode Nyata** — perbandingan harian, mingguan, dan bulanan memakai snapshot pada/ sebelum titik waktu pembanding
- **Differential Logging** — data kompatibilitas lama tetap dipelihara; snapshot immutable menjadi sumber analitik akurat
- **Web Dashboard** — akses via browser di `http://localhost:8787`
  - Top 10 Growing Folders (FR-03)
  - Grafik historis ukuran folder (FR-04)
  - Export data ke CSV (FR-06)
  - Health check endpoint `/healthz` (NFR-07)
- **Configurable Retention** — mode `aggregate` (ringkas mingguan) atau `delete` (hapus langsung)
- **Windows Service** — auto-start, recovery restart otomatis jika crash (NFR-06)
- **Multi-drive** — pantau beberapa drive sekaligus via `config.yaml`
- **Graceful Error Handling** — folder yang error di-skip, scan tetap lanjut (FR-08)
- **Panic Recovery** — setiap goroutine dibungkus `recover()`, panic tidak menjatuhkan proses (NFR-06)

## Cara Testing (Tanpa Install Service)

```bash
# Pastikan config.yaml sudah dikonfigurasi
go mod tidy
go run ./cmd/diskhist run
```

Buka browser ke `http://localhost:8787` atau cek `http://localhost:8787/healthz`.

> Catatan: Untuk testing lokal, ubah `database_path` dan `log_path` di `config.yaml`
> ke path lokal, misal `./diskhist.db` dan `./agent.log`.

## Paket rilis satu-folder (Windows)

Rilis resmi adalah satu ZIP yang berisi `diskhist.exe`, `config.yaml`, `README.md`,
`update-diskhist.bat`, dan `SHA256SUMS.txt`. Skrip packaging membuat build Windows
amd64, checksum binary, dan ZIP secara reproducible:

```bash
VERSION=1.0.0 SOURCE_DATE_EPOCH=0 ./scripts/package-release.sh
# hasil: dist/diskhist-windows-amd64-1.0.0.zip
```

### Instalasi dan update satu paket

1. Unduh `diskhist-windows-amd64-<versi>.zip` dan verifikasi checksum arsip menggunakan `dist/SHA256SUMS-ARCHIVE.txt` atau checksum rilis.
2. Ekstrak seluruh ZIP ke satu folder instalasi, misalnya `C:\Program Files\DiskHistMonitor\`. Jangan memindahkan atau mengganti nama file di dalam folder tersebut.
3. Edit `config.yaml` di folder yang sama, lalu buka PowerShell sebagai Administrator dan jalankan:

```powershell
cd 'C:\Program Files\DiskHistMonitor'
.\diskhist.exe install
sc.exe failure "DiskHistMonitor" reset= 86400 actions= restart/60000/restart/300000/restart/600000
sc.exe start DiskHistMonitor
```

4. Untuk update manual, ekstrak paket versi baru ke folder sementara, lalu salin `diskhist.exe`, `README.md`, dan `update-diskhist.bat` ke folder instalasi. Pertahankan `config.yaml` yang sudah dipakai instalasi (salin `config.yaml` dari paket hanya jika memang ingin mengganti konfigurasi). Jangan menimpa data runtime di `%ProgramData%\DiskHistMonitor`. Atau gunakan tombol **Update** di dashboard.
5. Tombol **Update** web hanya berfungsi jika `update-diskhist.bat` berada di folder yang sama dengan `diskhist.exe`; tombol tersebut menjalankan updater di sebelah executable. Updater mengunduh rilis terbaru, memverifikasi SHA-256, membuat backup, mengganti binary, dan melakukan rollback jika health check gagal.

Untuk penghapusan service:

```powershell
sc.exe stop DiskHistMonitor
.\diskhist.exe uninstall
```

### Build untuk produksi (Windows)

```bash
# Cross-compile langsung (untuk build manual; paket rilis memakai scripts/package-release.sh)
GOOS=windows GOARCH=amd64 /usr/local/go/bin/go build -o diskhist.exe ./cmd/diskhist
```

### Install sebagai Windows Service

```powershell
# Jalankan sebagai Administrator di PowerShell
.\diskhist.exe install

# Set auto-restart jika crash (NFR-06)
sc.exe failure "DiskHistMonitor" reset= 86400 actions= restart/60000/restart/300000/restart/600000

# Start service
sc.exe start DiskHistMonitor

# Verifikasi jalan
curl http://localhost:8787/healthz

# Uninstall
sc.exe stop DiskHistMonitor
.\diskhist.exe uninstall
```

## Konfigurasi

Lihat `config.yaml` untuk semua opsi yang tersedia:

| Parameter | Default | Keterangan |
|-----------|---------|------------|
| `drives` | `["D:\\"]` | Drive yang dipantau |
| `max_depth` | `5` | Folder/subfolder yang ditampilkan sampai kedalaman ini; semua isi yang lebih dalam tetap dihitung ke ukuran ancestor |
| `schedule` | `"0 2 * * *"` | Jadwal scan (cron format); jalankan minimal harian agar analisis harian tersedia |
| `retention.mode` | `"aggregate"` | Mode retensi: `aggregate` atau `delete` |
| `retention.retain_raw_days` | `90` | Data mentah disimpan; minimal 31 hari agar baseline bulanan tersedia |
| `accuracy.vss_mode` | `"required"` | Wajib VSS point-in-time; scan gagal eksplisit jika VSS tidak tersedia |
| `accuracy.collect_allocated_size` | `true` | Hitung juga Windows Properties “Size on disk” memakai API Windows |
| `resources.scan_timeout` | `"2h"` | Batas waktu scan satu volume |
| `resources.command_timeout` | `"2m"` | Batas waktu operasi create/delete VSS |
| `resources.single_scan` | `true` | Cegah batch scan terjadwal berjalan bersamaan |
| `web_server.port` | `8787` | Port web dashboard |
| `web_server.bind_address` | `"127.0.0.1"` | Hanya localhost (tidak bisa diakses dari LAN) |
| `database_path` | `C:\ProgramData\...` | Lokasi file SQLite |
| `log_path` | `C:\ProgramData\...` | Lokasi file log |

## Struktur Project

```
diskhist-monitor/
├── cmd/diskhist/main.go           # entry point
├── internal/
│   ├── config/config.go           # load & validasi config.yaml
│   ├── scanner/scanner.go         # recursive directory walk + differential logging
│   ├── storage/sqlite.go          # koneksi SQLite + migrasi schema
│   ├── retention/retention.go     # logic aggregate/delete retensi
│   ├── webserver/
│   │   ├── server.go              # HTTP server setup
│   │   ├── handlers.go            # API handlers
│   │   ├── dashboard.go           # embedded HTML dashboard
│   │   ├── status.go              # health status tracker
│   │   └── static/index.html      # dashboard UI
│   └── winservice/
│       ├── service.go             # Windows Service integration
│       └── service_stub.go        # non-Windows stub
├── config.yaml
├── go.mod
└── README.md
```

## Arsitektur

```
┌─────────────────────────────────────────┐
│           diskhist.exe (1 proses)       │
│                                         │
│  ┌──────────────┐  ┌──────────────────┐ │
│  │   Scanner    │  │   Web Server     │ │
│  │  (goroutine) │  │   (goroutine)    │ │
│  │              │  │  localhost:8787   │ │
│  │  WalkDir +   │  │  /healthz        │ │
│  │  Diff Log    │  │  /api/top-growers│ │
│  │              │  │  /api/export-csv │ │
│  └──────┬───────┘  └────────┬─────────┘ │
│         │                   │           │
│         └───────┬───────────┘           │
│                 │                       │
│         ┌───────▼───────┐               │
│         │  SQLite (WAL) │               │
│         │  scan_history │               │
│         │ folder_metrics│               │
│         └───────────────┘               │
│                                         │
│  ┌──────────────┐  ┌──────────────────┐ │
│  │  Retention   │  │   Cron Scheduler │ │
│  │  (03:00)     │  │   (robfig/cron)  │ │
│  └──────────────┘  └──────────────────┘ │
└─────────────────────────────────────────┘
```
