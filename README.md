# DiskHist Monitor

Satu binary Go (`diskhist.exe`) yang menjalankan scanner storage dan web dashboard
dalam satu proses, sebagai Windows Service.

## Fitur

- **Automated Background Scanner** — scan folder secara rekursif hingga kedalaman yang dikonfigurasi
- **Differential Logging** — hanya simpan perubahan ukuran, hemat ruang database
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

## Cara Build untuk Produksi (Windows)

```bash
# Cross-compile dari Linux/Mac
GOOS=windows GOARCH=amd64 go build -o diskhist.exe ./cmd/diskhist

# Atau build langsung di Windows
go build -o diskhist.exe ./cmd/diskhist
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
| `max_depth` | `5` | Kedalaman rekursi maksimum |
| `schedule` | `"0 2 * * *"` | Jadwal scan (cron format) |
| `retention.mode` | `"aggregate"` | Mode retensi: `aggregate` atau `delete` |
| `retention.retain_raw_days` | `30` | Berapa hari data mentah disimpan |
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
