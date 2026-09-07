# **Product Requirements Document (PRD) & Software Requirements Specification (SRS)**

**Nama Produk:** DiskHist Monitor

**Versi Dokumen:** 4.0 (FINAL — Self-Contained, Siap Langsung Dikerjakan AI Tanpa Pertanyaan Lanjutan)

**Tanggal:** 30 Juni 2026

**Target Pengguna:** System Administrator, DevOps, Cloud Engineer

**Catatan Penting:** Dokumen ini adalah spesifikasi final dan lengkap yang dimaksudkan sebagai panduan langsung bagi AI (Claude Code atau AI lain) untuk membangun produk dari nol. Semua keputusan arsitektur di bawah ini sudah dikunci — AI implementer tidak perlu menebak atau memilih alternatif, cukup mengikuti spesifikasi ini secara langsung. Prototype CLI/C++ sebelumnya (`diskhist.exe`) dianggap deprecated dan tidak digunakan lagi sebagai basis kode.

---

## **1. PRODUCT REQUIREMENTS DOCUMENT (PRD)**

### **1.1 Ringkasan Eksekutif**

Sistem mengalami lonjakan kapasitas penyimpanan yang signifikan secara berkala pada drive tertentu. Penambahan kapasitas storage secara terus-menerus berdampak buruk pada biaya operasional (baik on-premise maupun cloud seperti AWS EBS). DiskHist Monitor dibuat untuk memberikan visibilitas historis mengenai folder dan subfolder mana yang memakan ruang paling besar dari waktu ke waktu, sehingga tim dapat menemukan akar masalah (*root cause*) dan melakukan pembersihan yang tepat sasaran.

Drive yang dipantau **tidak di-hardcode** — ditentukan melalui file konfigurasi (lihat Bagian 2.5), sehingga aplikasi ini dapat dipakai untuk drive apa pun (D:\, E:\, atau drive lain) tanpa perlu rebuild.

### **1.2 Tujuan dan Sasaran (Objectives)**

* Mengidentifikasi direktori yang mengalami lonjakan kapasitas harian, mingguan, dan bulanan secara akurat.
* Menyediakan antarmuka visual (GUI) yang memudahkan analisis tanpa perlu mengeksekusi skrip manual setiap kali ingin melakukan pengecekan.
* Menjaga performa server Windows utama dengan memastikan proses pemindaian berjalan ringan di latar belakang tanpa memonopoli I/O disk.
* Memungkinkan penyesuaian perilaku aplikasi (drive target, jadwal, retensi, kedalaman scan) tanpa perlu mengubah atau mengompilasi ulang kode melalui file konfigurasi eksternal.

### **1.3 Metrik Keberhasilan (Success Metrics)**

* Akar penyebab pembengkakan disk dapat diidentifikasi dalam minggu pertama penggunaan.
* Konsumsi RAM aplikasi (proses background) stabil di bawah 100MB saat proses scanning berlangsung.
* Ukuran database lokal tetap terkendali sesuai kebijakan retensi yang diatur pengguna di file konfigurasi.
* Proses scanning tidak pernah berhenti total (crash) akibat error filesystem individual (file terkunci, reparse point, permission denied, dll).

### **1.4 Kebutuhan Fitur Utama (Epics)**

1. **Automated Background Scanner (Agent):** Modul independen yang memindai ukuran folder secara otomatis berdasarkan jadwal yang diatur melalui konfigurasi.
2. **Smart Differential Storage:** Sistem penyimpanan log efisien yang hanya mencatat *delta* (perubahan) untuk mencegah ukuran database membengkak seiring waktu.
3. **Configurable Data Retention:** Kebijakan retensi data (berapa lama data mentah disimpan, apakah diagregasi atau dihapus) diatur sepenuhnya melalui file konfigurasi, bukan hardcoded.
4. **Historical Analytics Dashboard:** Antarmuka visual desktop interaktif (read-only terhadap database) yang menampilkan grafik tren pertumbuhan folder dan daftar direktori dengan lonjakan terbesar (*Top Growers*).
5. **External Configuration File:** File konfigurasi (YAML) yang memungkinkan penyesuaian parameter inti aplikasi tanpa modifikasi kode.

---

## **2. SOFTWARE REQUIREMENTS SPECIFICATION (SRS)**

### **2.1 Arsitektur Sistem (Keputusan Final)**

Sistem ini menggunakan **satu binary tunggal** (`diskhist.exe`) yang menggabungkan scanner dan web dashboard dalam satu proses, berjalan sebagai Windows Service. Tidak ada lagi aplikasi desktop terpisah (Tauri/WPF) — dashboard diakses melalui browser ke `http://localhost:<port>`.

| Komponen | Teknologi | Alasan |
| :---- | :---- | :---- |
| **Binary utama** | **Go (Golang)** | Static binary tunggal tanpa dependency runtime eksternal (tidak ada lagi masalah DLL seperti `libstdc++`/`libgcc`), goroutine native untuk *directory traversal* paralel dan web server berjalan konkuren dalam proses yang sama. |
| **Scanner (goroutine internal)** | Berjalan sebagai goroutine terjadwal di dalam proses utama, bukan proses terpisah. | Menyederhanakan deployment — hanya satu proses untuk dimonitor/restart, tidak ada 2 service terpisah yang harus disinkronkan. |
| **Web Server (goroutine internal)** | `net/http` (Go standard library) atau framework ringan seperti `chi`/`gin`, bind ke `127.0.0.1:<port>` saja. | Dashboard diakses via browser lokal (`http://localhost:8787` misalnya). Tidak bind ke `0.0.0.0` — tidak bisa diakses dari device lain di jaringan, sesuai kebutuhan privasi/keamanan lokal. |
| **Penyimpanan** | **SQLite lokal** (dalam proses yang sama) | `PRAGMA auto_vacuum = INCREMENTAL` aktif, `PRAGMA journal_mode = WAL` aktif (lihat Bagian 2.4), indexing pada `folder_path` dan `depth_level`. |
| **Komunikasi Scanner ↔ Web Server** | **In-memory, dalam proses Go yang sama** (shared `*sql.DB` connection pool) | Karena keduanya goroutine dalam satu proses, tidak perlu IPC atau file-locking — cukup koneksi database SQLite yang dipakai bersama dengan WAL mode untuk concurrency baca/tulis yang aman. |
| **Penjadwalan** | **Windows Service** (bukan Task Scheduler) | Satu Windows Service menjalankan binary `diskhist.exe`. Service ini otomatis di-restart oleh Windows Service Control Manager (SCM) jika proses crash (`Recovery` action diset ke "Restart the Service" di properti service). Jadwal scan internal (goroutine) dikontrol oleh `config.yaml`, bukan dipicu eksternal oleh Task Scheduler. |
| **Keandalan proses** | **Windows Service Recovery + panic recovery internal** | Selain SCM auto-restart di level OS, setiap goroutine scanner dibungkus `recover()` di level Go agar panic pada satu goroutine (misal saat scan) **tidak menjatuhkan seluruh proses** (web server tetap hidup walau scan gagal). Lihat NFR-04 dan NFR-06. |

### **2.2 Kebutuhan Fungsional (Functional Requirements)**

| ID | Fitur | Deskripsi |
| :---- | :---- | :---- |
| **FR-01** | Rekursi Kedalaman Folder | Agent memindai struktur folder hingga kedalaman maksimum sesuai `max_depth` di konfigurasi (default: 5). Semua ukuran subfolder di bawah level maksimum dikalkulasi dan digabungkan ke folder induk pada level tersebut. |
| **FR-02** | Differential Logging | Sistem tidak membuat record baru jika tidak ada perubahan ukuran dibanding pemindaian sebelumnya. Sistem hanya memperbarui `end_date`. |
| **FR-03** | Visualisasi Top Growers | Dashboard menampilkan tabel 10 folder dengan persentase dan volume pertumbuhan absolut tertinggi dalam rentang waktu tertentu. |
| **FR-04** | Grafik Historis | Dashboard menyediakan *line chart* yang memvisualisasikan tren ukuran sebuah folder spesifik berdasarkan rentang tanggal yang dipilih. |
| **FR-05** | Retensi & Agregasi Data | Sistem menjalankan kebijakan retensi sesuai parameter `retention` di konfigurasi (lihat Bagian 2.5). Mode retensi (`aggregate` atau `delete`) ditentukan oleh pengguna melalui konfigurasi, **bukan dikunci ke satu metode oleh sistem**. |
| **FR-06** | Ekspor Laporan | Pengguna dapat mengekspor data metrik folder dan daftar Top Growers ke format CSV. |
| **FR-07** | External Configuration | Seluruh parameter inti (drive target, jadwal scan, `max_depth`, kebijakan retensi, **port web server**) dibaca dari file `config.yaml` saat proses startup. Perubahan konfigurasi tidak memerlukan rebuild aplikasi, namun memerlukan restart Windows Service untuk diterapkan. |
| **FR-08** | Graceful Error Handling per Folder | Jika terjadi error saat mengakses folder/file tertentu (permission denied, reparse point rusak, path terlalu panjang, dll), proses mencatat (log) dan **melewati** folder tersebut, lalu melanjutkan scan ke folder berikutnya tanpa menghentikan seluruh proses. |
| **FR-09** | Web Server Lifecycle | Web server harus mulai listening di `127.0.0.1:<port>` segera saat Windows Service start, independen dari status scanner. Jika scan sedang berjalan atau gagal, dashboard tetap bisa diakses dan menampilkan data terakhir yang valid dari database. |
| **FR-10** | Startup Self-Check | Saat proses pertama kali start, sistem melakukan pengecekan dasar (port tidak dipakai proses lain, folder `database_path` dan `log_path` bisa ditulis) dan mencatat hasilnya ke log. Jika port sudah dipakai, proses mencatat error spesifik dan tetap mencoba scanner berjalan (web server boleh gagal start tanpa menjatuhkan scanner, dan sebaliknya). |

### **2.3 Kebutuhan Non-Fungsional (Non-Functional Requirements)**

| ID | Parameter | Spesifikasi |
| :---- | :---- | :---- |
| **NFR-01** | Kinerja (Waktu Eksekusi) | Proses scanning drive sebesar 1TB dengan struktur kompleks harus selesai dalam waktu maksimal 5-10 menit, **tanpa membuat web server menjadi tidak responsif** (scan berjalan di goroutine terpisah dari HTTP handler). |
| **NFR-02** | Keamanan I/O Disk | Proses scanner (goroutine di dalam `diskhist.exe`) berjalan dengan prioritas I/O rendah (menggunakan `SetPriorityClass` / `IoPriorityHint` di level proses Windows, diterapkan ke proses utama). |
| **NFR-03** | Kapasitas Database | Ukuran database dikendalikan oleh kebijakan retensi yang dikonfigurasi pengguna (lihat FR-05); sistem tidak memaksakan satu target ukuran tetap. |
| **NFR-04** | Keandalan (Reliability) — Level Folder | Sesuai FR-08 — proses tidak boleh berhenti total karena error pada satu folder/file. Semua exception filesystem ditangkap, dicatat, dan dilewati secara halus. |
| **NFR-05** | Portabilitas Konfigurasi | File `config.yaml` harus dapat diedit langsung oleh pengguna menggunakan text editor apa pun, tanpa memerlukan tools khusus, dan divalidasi saat startup (jika invalid, proses mencatat error spesifik dan tidak menjalankan scan dengan asumsi default yang membahayakan data). |
| **NFR-06** | Keandalan (Reliability) — Level Proses | Proses utama (`diskhist.exe`) tidak boleh mati total akibat panic di satu goroutine. Setiap goroutine scan dan setiap HTTP handler dibungkus `recover()`. Jika seluruh proses tetap crash karena sebab di luar kendali (misal out-of-memory), Windows Service Control Manager harus dikonfigurasi dengan **Recovery action: "Restart the Service"** (delay restart pertama: 1 menit, kedua: 5 menit, berikutnya: 10 menit — mengikuti praktik standar Windows Service recovery) agar service kembali hidup otomatis tanpa intervensi manual. |
| **NFR-07** | Verifikasi "Benar-Benar Jalan" | Sistem menyediakan endpoint `GET /healthz` pada web server yang mengembalikan status proses (uptime, waktu scan terakhir, status scan terakhir, jumlah folder ter-skip). Endpoint ini dipakai untuk verifikasi manual maupun otomatis bahwa service benar-benar berjalan, bukan hanya "service status: running" di Windows yang belum tentu mencerminkan proses internal sehat. |
| **NFR-08** | Logging yang Spesifik & Actionable | Setiap log error WAJIB memuat: (1) konteks operasi (misal "scan folder X"), (2) path/file spesifik yang terlibat, (3) pesan error asli dari Go (`err.Error()`), dan (4) jika memungkinkan, saran tindakan. **Dilarang** menulis log generik seperti `"Error occurred"` atau `"Something went wrong"` tanpa detail. Tujuannya supaya orang yang tidak familiar dengan Go tetap bisa memahami akar masalah hanya dari membaca log, tanpa perlu membaca source code. Level log yang dipakai: `INFO` (operasi normal: scan mulai/selesai, service start), `WARN` (folder di-skip karena error, tapi proses lanjut), `ERROR` (kegagalan yang mempengaruhi fungsi utama, misal database tidak bisa ditulis), `FATAL` (proses harus berhenti, misal config invalid saat startup). |

### **2.4 Skema Pangkalan Data (SQLite Data Model)**

> **Catatan implementasi:** Database harus dibuka dengan **WAL (Write-Ahead Logging) mode** (`PRAGMA journal_mode=WAL`) agar Dashboard (reader) dapat membaca database secara konkuren tanpa terblokir saat Agent (writer) sedang menulis data.

**Tabel 1: scan_history** *(Mencatat riwayat eksekusi pemindaian)*

| Nama Kolom | Tipe Data | Keterangan |
| :---- | :---- | :---- |
| id | INTEGER | Primary Key (Auto Increment) |
| scan_date | DATETIME | Waktu pemindaian selesai dijalankan |
| drive_letter | TEXT | Drive yang dipindai pada eksekusi ini (dibaca dari config) |
| total_size_bytes | BIGINT | Total keseluruhan ukuran drive saat itu |
| status | TEXT | `success`, `partial` (ada folder yang dilewati karena error), atau `failed` |
| skipped_count | INTEGER | Jumlah folder/file yang dilewati karena error (terkait FR-08) |

**Tabel 2: folder_metrics** *(Catatan historis untuk Differential Logging)*

| Nama Kolom | Tipe Data | Keterangan |
| :---- | :---- | :---- |
| id | INTEGER | Primary Key (Auto Increment) |
| folder_path | TEXT | Lokasi absolut direktori — **INDEXED** |
| depth_level | INTEGER | Level kedalaman subfolder (1 hingga `max_depth`) — **INDEXED** |
| size_bytes | BIGINT | Ukuran folder yang terekam |
| start_date | DATETIME | Waktu (scan) pertama kali ukuran spesifik ini tercatat |
| end_date | DATETIME | Waktu (scan) terakhir ukuran ini valid sebelum akhirnya berubah, atau NULL jika masih berlaku |
| is_active | BOOLEAN | `true` jika folder masih ditemukan pada scan terakhir; diset `false` jika folder tidak lagi ditemukan (terhapus dari disk) — record lama disegel, tidak dihapus dari database |

**Catatan Implementasi Differential Logging:**
Alih-alih membuat baris baru setiap kali scan, sistem hanya memperbarui `end_date` jika ukuran folder pada scan ini sama persis dengan scan sebelumnya. Jika ukurannya berbeda, sistem menyegel `end_date` pada record lama, lalu menyisipkan baris baru dengan `size_bytes` baru dan `start_date` hari ini.

**Penanganan folder yang dihapus:** Jika sebuah `folder_path` yang sebelumnya aktif tidak lagi ditemukan pada scan terbaru, sistem menyegel `end_date` record tersebut ke waktu scan saat ini dan mengubah `is_active` menjadi `false`. Record ini tidak dihapus — tetap tersimpan sebagai riwayat historis bahwa folder tersebut pernah ada dan kapan terakhir terlihat.

### **2.5 File Konfigurasi (config.yaml)**

Format YAML dipilih karena mudah dibaca, mendukung komentar inline, dan menjadi standar umum untuk konfigurasi aplikasi semacam ini. File ini dibaca oleh Agent saat startup (Windows Service start/restart).

```yaml
# config.yaml — DiskHist Monitor Agent Configuration

# Drive yang akan dipantau. Bisa lebih dari satu.
drives:
  - "D:\\"
  # - "E:\\"   # uncomment untuk memantau drive tambahan

# Kedalaman maksimum rekursi folder.
# Subfolder di bawah level ini ukurannya digabung ke folder induk pada level ini.
max_depth: 5

# Jadwal scan otomatis, format cron-like (menit jam * * *)
# Contoh di bawah: setiap hari pukul 02:00
schedule: "0 2 * * *"

# Kebijakan retensi data historis.
# mode: "aggregate" -> data mentah lebih tua dari retain_raw_days diringkas jadi mingguan/bulanan
# mode: "delete"     -> data mentah lebih tua dari retain_raw_days langsung dihapus, tanpa agregasi
retention:
  mode: "aggregate"     # "aggregate" atau "delete", dipilih oleh pengguna
  retain_raw_days: 90    # minimum 31 hari agar analisis bulanan tetap tersedia

# Port web server dashboard (hanya bind ke localhost/127.0.0.1, tidak ke LAN)
web_server:
  port: 8787
  bind_address: "127.0.0.1"   # sengaja tidak 0.0.0.0 — hanya bisa diakses dari komputer ini sendiri

# Lokasi file database SQLite
database_path: "C:\\ProgramData\\DiskHistMonitor\\diskhist.db"

# Lokasi file log Agent
log_path: "C:\\ProgramData\\DiskHistMonitor\\agent.log"
```

**Validasi saat startup:** Jika `config.yaml` tidak ditemukan, tidak valid (syntax error), atau memuat nilai yang tidak masuk akal (misalnya `max_depth: 0` atau drive letter yang tidak ada), Agent **tidak boleh** menjalankan scan dengan asumsi default secara diam-diam. Agent harus mencatat error spesifik ke `log_path` dan menghentikan startup, agar pengguna tahu pasti konfigurasi mana yang harus diperbaiki.

---

## **3. RINGKASAN KEPUTUSAN YANG SUDAH DIKUNCI**

Bagian ini merangkum semua keputusan desain yang sudah final, agar AI implementer tidak perlu menebak ulang:

1. Bahasa & arsitektur: **Go, satu binary tunggal** (`diskhist.exe`) — scanner dan web dashboard jadi goroutine dalam proses yang sama, bukan dua proses/aplikasi terpisah.
2. Dashboard: **web server lokal** diakses via browser (`http://localhost:8787`), bukan aplikasi desktop terpisah (Tauri/WPF dibatalkan).
3. Akses jaringan: **localhost saja** (`127.0.0.1`), sengaja tidak bisa diakses dari device lain di LAN.
4. Penjadwalan: **Windows Service tunggal** menjalankan binary gabungan, bukan Windows Task Scheduler eksternal, bukan dua service terpisah.
5. Keandalan: **dua lapis** — (a) `recover()` di setiap goroutine/handler Go agar panic lokal tidak menjatuhkan proses, (b) Windows Service Recovery action "Restart the Service" sebagai fallback level OS jika proses tetap mati total.
6. Verifikasi proses sehat: endpoint **`GET /healthz`** wajib ada, supaya "jalan" bisa diverifikasi secara konkret (uptime, scan terakhir), bukan cuma asumsi dari status service Windows.
7. Drive target: **dikonfigurasi**, mendukung lebih dari satu drive, tidak hardcode ke satu drive letter.
8. Retensi data: **dikonfigurasi** (`aggregate` atau `delete`), bukan dikunci ke satu metode oleh sistem.
9. Format konfigurasi: **YAML**, file bernama `config.yaml`, termasuk port web server.
10. Error handling filesystem: **per-folder skip**, tidak pernah crash total (lihat FR-08 / NFR-04).
11. Prototype lama (`diskhist.exe` versi C++/MinGW CLI) **tidak digunakan lagi** sebagai basis kode — pembangunan dimulai dari nol sesuai spesifikasi ini. (Catatan: nama file `diskhist.exe` dipakai ulang untuk binary baru, tapi isinya sepenuhnya berbeda — Go, bukan C++.)
12. Logging: **wajib spesifik dan actionable** (lihat NFR-08) — tidak boleh ada log generik tanpa konteks.
13. Bahasa pemrograman: **Go dikonfirmasi final, bukan Electron** — alasan lengkap: binary statis tanpa dependency runtime, footprint RAM rendah (cocok untuk background service 24/7), error handling eksplisit yang predictable untuk debugging, dan track record kuat untuk infrastruktur/daemon. Electron tidak dipakai karena dirancang untuk aplikasi desktop yang dibuka user (bukan invisible background service), footprint RAM/disk jauh lebih besar, dan memerlukan wrapper tambahan untuk berjalan sebagai Windows Service.

---

## **4. STRUKTUR PROJECT & PANDUAN BUILD (Untuk AI Implementer)**

Bagian ini memastikan AI implementer dapat langsung mulai coding tanpa bertanya balik soal struktur project atau cara build.

### **4.1 Struktur Folder**

```
diskhist-monitor/
├── cmd/
│   └── diskhist/
│       └── main.go              # entry point, setup service + web server + scanner
├── internal/
│   ├── config/
│   │   └── config.go            # load & validate config.yaml (Viper atau gopkg.in/yaml.v3)
│   ├── scanner/
│   │   └── scanner.go           # logic recursive directory walk, differential logging
│   ├── storage/
│   │   └── sqlite.go            # koneksi SQLite, migrasi schema, query helper
│   ├── retention/
│   │   └── retention.go         # logic aggregate/delete sesuai config
│   ├── webserver/
│   │   ├── server.go            # setup net/http server, routing
│   │   ├── handlers.go          # handler untuk /healthz, /api/top-growers, /api/export-csv, dll
│   │   └── static/              # HTML/CSS/JS sederhana untuk dashboard (atau embed.FS)
│   └── winservice/
│       └── service.go           # integrasi Windows Service (golang.org/x/sys/windows/svc)
├── config.yaml                  # contoh config default (lihat Bagian 2.5)
├── go.mod
├── go.sum
└── README.md                    # cara build, install service, uninstall service
```

### **4.2 Library Go yang Direkomendasikan**

| Kebutuhan | Library | Alasan |
| :---- | :---- | :---- |
| Windows Service | `golang.org/x/sys/windows/svc` | Library resmi Go untuk integrasi Windows Service Control Manager (SCM). |
| SQLite driver | `github.com/mattn/go-sqlite3` atau `modernc.org/sqlite` | `modernc.org/sqlite` direkomendasikan jika ingin **pure Go tanpa CGO** — menghindari kebutuhan compiler C sama sekali saat build, konsisten dengan tujuan "tidak ada masalah dependency lagi". |
| Parsing YAML | `gopkg.in/yaml.v3` | Standar, ringan, cukup untuk kebutuhan `config.yaml` ini. |
| HTTP routing | `net/http` standar Go, atau `github.com/go-chi/chi/v5` jika butuh routing lebih rapi | Hindari framework berat; kebutuhan endpoint di sini sederhana. |
| Cron scheduling internal | `github.com/robfig/cron/v3` | Untuk parsing format `schedule` cron-like di `config.yaml` dan menjalankan scan terjadwal di goroutine. |

### **4.3 Cara Build (untuk dimasukkan ke README.md)**

```bash
# Build untuk Windows (dari Windows atau cross-compile dari Linux/Mac)
GOOS=windows GOARCH=amd64 go build -o diskhist.exe ./cmd/diskhist

# Install sebagai Windows Service (jalankan sebagai Administrator)
diskhist.exe install

# Set Recovery action auto-restart (lihat NFR-06) — via sc.exe atau PowerShell
sc.exe failure "DiskHistMonitor" reset= 86400 actions= restart/60000/restart/300000/restart/600000

# Start service
sc.exe start DiskHistMonitor

# Verifikasi jalan (lihat NFR-07)
curl http://localhost:8787/healthz
```

### **4.4 Definisi "Selesai" untuk MVP (Minimum Viable Product)**

AI implementer dapat menganggap implementasi awal selesai jika kondisi berikut terpenuhi, tanpa perlu bertanya lebih lanjut soal scope:

1. Binary `diskhist.exe` bisa di-install sebagai Windows Service dan berjalan tanpa crash selama proses scan drive sungguhan (minimal 10.000 folder, untuk uji ketahanan terhadap FR-08).
2. `GET /healthz` mengembalikan JSON valid dengan field `uptime_seconds`, `last_scan_time`, `last_scan_status`, `skipped_count`.
3. Dashboard web (halaman HTML sederhana, tidak perlu framework frontend modern/React di awal — cukup HTML + sedikit JS untuk fetch data dari endpoint JSON) menampilkan minimal: tabel Top 10 Growing Folders (FR-03) dan tombol export CSV (FR-06).
4. `config.yaml` sesuai Bagian 2.5 berhasil di-parse, dan jika file tidak ada/invalid, service mencatat FATAL log dan tidak menjalankan scan (sesuai NFR-05).
5. Skema database sesuai Bagian 2.4 diterapkan dengan migrasi otomatis saat pertama kali start (jika tabel belum ada, buat otomatis — tidak perlu tool migrasi eksternal terpisah untuk versi awal ini).

Fitur lanjutan (line chart historis FR-04, agregasi retensi penuh FR-05) dapat dikerjakan setelah MVP di atas terverifikasi jalan stabil.
