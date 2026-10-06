// Package main adalah entry point diskhist.exe.
//
// Sesuai PRD/SRS Bagian 2.1: satu binary tunggal yang menjalankan scanner
// (goroutine terjadwal) dan web server (goroutine net/http) dalam satu
// proses Windows Service.
//
// Mode menjalankan:
//
//	diskhist.exe run              -> jalan langsung di foreground (untuk testing manual)
//	diskhist.exe install          -> install sebagai Windows Service
//	diskhist.exe uninstall        -> uninstall Windows Service
//
// Tanpa argumen di Windows: deteksi apakah dipanggil oleh SCM, jika ya
// masuk mode service, jika tidak tampilkan usage.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"diskhist-monitor/internal/config"
	"diskhist-monitor/internal/report"
	"diskhist-monitor/internal/retention"
	"diskhist-monitor/internal/scanner"
	"diskhist-monitor/internal/storage"
	"diskhist-monitor/internal/webserver"
	"diskhist-monitor/internal/winservice"

	"github.com/robfig/cron/v3"
)

const configPath = "config.yaml"

func executableConfigPath(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	return config.ResolveConfigPath(name, exe)
}

func main() {
	// Tanpa argumen: cek apakah dijalankan oleh Windows SCM
	if len(os.Args) < 2 {
		if winservice.IsServiceMode() {
			runAsWindowsService()
			return
		}
		printUsage()
		os.Exit(1)
	}

	switch os.Args[1] {
	case "run":
		runForeground()
	case "report":
		runReport(os.Args[2:])
	case "install":
		if err := winservice.InstallService(); err != nil {
			log.Fatalf("[FATAL] gagal install service: %v", err)
		}
	case "uninstall":
		if err := winservice.UninstallService(); err != nil {
			log.Fatalf("[FATAL] gagal uninstall service: %v", err)
		}
	case "help", "--help", "-h":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "mode %q tidak dikenal.\n\n", os.Args[1])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println("DiskHist Monitor — Automated disk usage history scanner")
	fmt.Println()
	fmt.Println("Usage:")
	fmt.Println("  diskhist.exe run         Jalankan di foreground (untuk testing)")
	fmt.Println("  diskhist.exe report      Scan lalu kirim hasilnya ke server Ndelik")
	fmt.Println("  diskhist.exe install     Install sebagai Windows Service")
	fmt.Println("  diskhist.exe uninstall   Uninstall Windows Service")
	fmt.Println()
	fmt.Println("Report ke Ndelik:")
	fmt.Println("  diskhist.exe report --server URL --api-key KEY [--config config.yaml] [--drive D:\\]")
	fmt.Println()
	fmt.Println("Konfigurasi: config.yaml di direktori yang sama dengan executable")
}

// runAsWindowsService menjalankan logic yang sama dengan runForeground()
// tapi melalui Windows Service Control Manager.
func runAsWindowsService() {
	err := winservice.RunAsService(func(ctx context.Context) {
		runCore(ctx)
	})
	if err != nil {
		log.Fatalf("[FATAL] gagal menjalankan Windows Service: %v", err)
	}
}

// runForeground menjalankan scanner + web server langsung di proses ini.
func runForeground() {
	runCore(context.Background())
}

// runCore menjalankan seluruh logic inti: config → DB → scanner → webserver → cron.
// Parent context berasal dari SCM pada service mode dan dibatalkan saat Stop.
func runCore(parent context.Context) {
	cfg, err := config.Load(executableConfigPath(configPath))
	if err != nil {
		log.Fatalf("[FATAL] gagal load config: %v", err)
	}

	// Setup log output ke file + stdout
	setupLogging(cfg.LogPath)

	log.Printf("[INFO] config berhasil di-load: drives=%v, max_depth=%d, port=%d, retention=%s (%d hari)",
		cfg.Drives, cfg.MaxDepth, cfg.WebServer.Port, cfg.Retention.Mode, cfg.Retention.RetainRawDays)

	// Pastikan direktori database ada
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		log.Fatalf("[FATAL] gagal membuat direktori database %q: %v", filepath.Dir(cfg.DBPath), err)
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[FATAL] gagal membuka database: %v", err)
	}
	defer store.Close()
	var reportWorker *outboxWorker
	if cfg.Ndelik.Enabled {
		apiKey, secretErr := resolveSecret(cfg.Ndelik.APIKeyRef, filepath.Dir(executableConfigPath(configPath)))
		if secretErr != nil {
			log.Fatalf("[FATAL] report Ndelik enabled but API key unavailable: %v", secretErr)
		}
		reportWorker = newReportWorker(store.DB(), report.NewClient(cfg.Ndelik.Server, apiKey), 5*time.Second, 8)
		go reportWorker.Run(parent)
	}

	status := webserver.NewStatusTracker()
	srv := webserver.New(cfg.WebServer.BindAddress, cfg.WebServer.Port, status, store.DB())
	scanCtx, cancelScans := context.WithCancel(parent)
	var scanWG sync.WaitGroup

	// Web server berjalan di goroutine sendiri (FR-09: independen dari scanner).
	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("[ERROR] web server gagal berjalan: %v", err)
		}
	}()

	scn := scanner.NewWithOptions(store.DB(), cfg.MaxDepth, scanner.Options{
		UseVSS:           cfg.Accuracy.VSSMode == "required",
		CollectAllocated: cfg.Accuracy.CollectAllocatedSize,
		ScanTimeout:      cfg.Resources.ScanTimeout,
		CommandTimeout:   cfg.Resources.CommandTimeout,
	})
	scn.SetAgentID(cfg.Ndelik.AgentID)
	ret := retention.New(store.DB(), cfg.Retention.Mode, cfg.Retention.RetainRawDays)
	var scanBatchRunning atomic.Bool

	// runScanSafely membungkus pemanggilan scanner dengan recover(),
	// sesuai NFR-06.
	runScanSafely := func() {
		if !scanBatchRunning.CompareAndSwap(false, true) {
			log.Printf("[WARN] jadwal scan dilewati karena batch scan sebelumnya masih berjalan")
			return
		}
		defer scanBatchRunning.Store(false)
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ERROR] panic ditangkap saat menjalankan scan: %v (proses tetap berjalan, web server tidak terpengaruh)", rec)
			}
		}()
		for _, drive := range cfg.Drives {
			result := scn.ScanDriveContext(scanCtx, drive)
			status.RecordScan(result.ScanDate, result.Status, result.SkippedCount)
		}
	}

	// Jalankan satu kali saat startup
	scanWG.Add(1)
	go func() { defer scanWG.Done(); runScanSafely() }()

	// Scheduler cron untuk scan terjadwal
	c := cron.New()
	_, err = c.AddFunc(cfg.Schedule, func() {
		scanWG.Add(1)
		defer scanWG.Done()
		runScanSafely()
	})
	if err != nil {
		log.Fatalf("[FATAL] format 'schedule' di config.yaml tidak valid (%q): %v", cfg.Schedule, err)
	}

	// Scheduler retensi: jalankan sekali sehari pukul 03:00
	_, err = c.AddFunc("0 3 * * *", func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ERROR] panic ditangkap saat menjalankan retensi: %v", rec)
			}
		}()
		if err := ret.Run(); err != nil {
			log.Printf("[ERROR] retensi gagal: %v", err)
		}
	})
	if err != nil {
		log.Fatalf("[FATAL] gagal menjadwalkan retensi: %v", err)
	}

	c.Start()

	log.Println("[INFO] diskhist berjalan. Tekan Ctrl+C untuk berhenti (mode foreground).")

	// Graceful shutdown: foreground menunggu OS signal; service menunggu SCM context.
	stopCh := make(chan struct{})
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(sigCh)
		select {
		case <-sigCh:
		case <-parent.Done():
		}
		close(stopCh)
	}()
	<-stopCh

	log.Println("[INFO] menerima sinyal stop, membatalkan scheduler dan scan aktif...")
	cronStopped := c.Stop()
	cancelScans()
	select {
	case <-cronStopped.Done():
	case <-time.After(cfg.Resources.CommandTimeout + 30*time.Second):
		log.Printf("[ERROR] timeout menunggu job scheduler berhenti")
	}
	scanDone := make(chan struct{})
	go func() { scanWG.Wait(); close(scanDone) }()
	select {
	case <-scanDone:
		log.Println("[INFO] scan aktif berhenti dan cleanup selesai")
	case <-time.After(cfg.Resources.CommandTimeout + 30*time.Second):
		log.Printf("[ERROR] timeout menunggu scan/cleanup VSS; proses akan melanjutkan shutdown")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[WARN] web server tidak shutdown dengan bersih dalam 10s: %v", err)
	}
	if reportWorker != nil {
		reportWorker.Stop()
	}
	log.Println("[INFO] shutdown selesai.")
}

// setupLogging mengarahkan log output ke file dan stdout secara bersamaan.
func setupLogging(logPath string) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		log.Printf("[WARN] gagal membuat direktori log %q: %v (akan log ke stdout saja)", filepath.Dir(logPath), err)
		return
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("[WARN] gagal membuka file log %q: %v (akan log ke stdout saja)", logPath, err)
		return
	}

	// Tulis ke stdout DAN file
	multiWriter := io.MultiWriter(os.Stdout, f)
	log.SetOutput(multiWriter)
	log.Printf("[INFO] log ditulis ke %s", logPath)
}

// runReport menjalankan scan sekali persis seperti mode run, lalu memuat hasil
// terbaru dari SQLite dan mengirimnya ke server Ndelik (POST /agent/v1/storage).
// Keluar dengan kode non-nol jika ada scan atau push yang gagal.
func runReport(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	server := fs.String("server", "", "URL server Ndelik, contoh https://ndelik.srbyte.id")
	apiKey := fs.String("api-key", "", "API key agent untuk autentikasi ke server Ndelik")
	cfgPath := fs.String("config", configPath, "Path file config.yaml")
	driveFlag := fs.String("drive", "", "Batasi scan ke satu drive (misal D:\\); default semua drive di config")
	fs.Usage = func() {
		fmt.Println("Usage: diskhist.exe report --server URL --api-key KEY [--config config.yaml] [--drive D:\\]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *server == "" || *apiKey == "" {
		fmt.Fprintln(os.Stderr, "flag --server dan --api-key wajib diisi")
		fs.Usage()
		os.Exit(1)
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("[FATAL] gagal load config: %v", err)
	}
	setupLogging(cfg.LogPath)
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0755); err != nil {
		log.Fatalf("[FATAL] gagal membuat direktori database %q: %v", filepath.Dir(cfg.DBPath), err)
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("[FATAL] gagal membuka database: %v", err)
	}
	defer store.Close()

	drives := cfg.Drives
	if *driveFlag != "" {
		drives = []string{*driveFlag}
	}

	scn := scanner.NewWithOptions(store.DB(), cfg.MaxDepth, scanner.Options{
		UseVSS:           cfg.Accuracy.VSSMode == "required",
		CollectAllocated: cfg.Accuracy.CollectAllocatedSize,
		ScanTimeout:      cfg.Resources.ScanTimeout,
		CommandTimeout:   cfg.Resources.CommandTimeout,
	})
	client := report.NewClient(*server, *apiKey)
	ctx := context.Background()

	failed := false
	for _, drive := range drives {
		func(drive string) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Printf("[ERROR] panic ditangkap saat report drive %q: %v", drive, rec)
					failed = true
				}
			}()
			result := scn.ScanDriveContext(ctx, drive)
			if result.Status == "failed" {
				log.Printf("[ERROR] scan drive %q gagal; report dilewati", drive)
				failed = true
				return
			}
			payload, err := report.LoadLatest(ctx, store.DB(), drive)
			if err != nil {
				log.Printf("[ERROR] drive %q: %v", drive, err)
				failed = true
				return
			}
			ack, err := client.Push(ctx, payload)
			if err != nil {
				log.Printf("[ERROR] push report drive %q gagal: %v", drive, err)
				failed = true
				return
			}
			log.Printf("[INFO] report drive %q terkirim: status=%s consistency=%s folders=%d accepted=%d scan_id=%s",
				drive, payload.Scan.Status, payload.Scan.Consistency, len(payload.Folders), ack.Accepted, ack.ScanID)
		}(drive)
	}
	if failed {
		os.Exit(1)
	}
}
