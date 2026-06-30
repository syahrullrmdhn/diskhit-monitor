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
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"diskhist-monitor/internal/config"
	"diskhist-monitor/internal/retention"
	"diskhist-monitor/internal/scanner"
	"diskhist-monitor/internal/storage"
	"diskhist-monitor/internal/webserver"
	"diskhist-monitor/internal/winservice"

	"github.com/robfig/cron/v3"
)

const configPath = "config.yaml"

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
	fmt.Println("  diskhist.exe install     Install sebagai Windows Service")
	fmt.Println("  diskhist.exe uninstall   Uninstall Windows Service")
	fmt.Println()
	fmt.Println("Konfigurasi: config.yaml di direktori yang sama dengan executable")
}

// runAsWindowsService menjalankan logic yang sama dengan runForeground()
// tapi melalui Windows Service Control Manager.
func runAsWindowsService() {
	err := winservice.RunAsService(func() {
		runCore()
	})
	if err != nil {
		log.Fatalf("[FATAL] gagal menjalankan Windows Service: %v", err)
	}
}

// runForeground menjalankan scanner + web server langsung di proses ini.
func runForeground() {
	runCore()
}

// runCore menjalankan seluruh logic inti: config → DB → scanner → webserver → cron.
// Dipanggil baik dari mode foreground maupun Windows Service.
func runCore() {
	cfg, err := config.Load(configPath)
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

	status := webserver.NewStatusTracker()
	srv := webserver.New(cfg.WebServer.BindAddress, cfg.WebServer.Port, status, store.DB())

	// Web server berjalan di goroutine sendiri (FR-09: independen dari scanner).
	go func() {
		if err := srv.Start(); err != nil {
			log.Printf("[ERROR] web server gagal berjalan: %v", err)
		}
	}()

	scn := scanner.New(store.DB(), cfg.MaxDepth)
	ret := retention.New(store.DB(), cfg.Retention.Mode, cfg.Retention.RetainRawDays)

	// runScanSafely membungkus pemanggilan scanner dengan recover(),
	// sesuai NFR-06.
	runScanSafely := func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ERROR] panic ditangkap saat menjalankan scan: %v (proses tetap berjalan, web server tidak terpengaruh)", rec)
			}
		}()
		for _, drive := range cfg.Drives {
			result := scn.ScanDrive(drive)
			status.RecordScan(result.ScanDate, result.Status, result.SkippedCount)
		}
	}

	// Jalankan satu kali saat startup
	go runScanSafely()

	// Scheduler cron untuk scan terjadwal
	c := cron.New()
	_, err = c.AddFunc(cfg.Schedule, runScanSafely)
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
	defer c.Stop()

	log.Println("[INFO] diskhist berjalan. Tekan Ctrl+C untuk berhenti (mode foreground).")

	// Graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	log.Println("[INFO] menerima sinyal stop, melakukan graceful shutdown...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("[WARN] web server tidak shutdown dengan bersih dalam 10s: %v", err)
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
