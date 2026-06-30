// Package retention mengimplementasikan FR-05: kebijakan retensi data
// historis sesuai konfigurasi (mode "aggregate" atau "delete").
//
// Mode "delete": hapus baris folder_metrics yang end_date lebih tua
// dari retain_raw_days dan is_active = 0.
//
// Mode "aggregate": sebelum hapus data mentah yang lebih tua dari
// retain_raw_days, ringkas dulu jadi rata-rata mingguan/bulanan ke
// tabel folder_metrics_weekly, baru hapus data mentahnya.
package retention

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

type Retention struct {
	db            *sql.DB
	mode          string
	retainRawDays int
}

func New(db *sql.DB, mode string, retainRawDays int) *Retention {
	return &Retention{db: db, mode: mode, retainRawDays: retainRawDays}
}

// Run menjalankan kebijakan retensi sesuai mode yang dikonfigurasi.
func (r *Retention) Run() error {
	switch r.mode {
	case "delete":
		return r.runDelete()
	case "aggregate":
		return r.runAggregate()
	default:
		return nil // sudah divalidasi di config.Load, seharusnya tidak terjadi
	}
}

// runDelete menghapus data mentah yang lebih tua dari retain_raw_days.
// Hanya menghapus record yang sudah tidak aktif (is_active = 0) dan
// end_date sudah lewat batas retensi.
func (r *Retention) runDelete() error {
	cutoff := time.Now().AddDate(0, 0, -r.retainRawDays)
	log.Printf("[INFO] retensi mode 'delete': menghapus data mentah sebelum %s", cutoff.Format("2006-01-02"))

	result, err := r.db.Exec(
		`DELETE FROM folder_metrics WHERE is_active = 0 AND end_date IS NOT NULL AND end_date < ?`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("gagal menghapus data retensi: %w", err)
	}

	rows, _ := result.RowsAffected()
	log.Printf("[INFO] retensi selesai: %d baris data lama dihapus", rows)

	// VACUUM incremental untuk reclaim space
	if _, err := r.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		log.Printf("[WARN] gagal menjalankan incremental_vacuum setelah retensi: %v", err)
	}

	return nil
}

// runAggregate meringkas data mentah mingguan/bulanan ke tabel aggregasi,
// lalu menghapus data mentah yang sudah diagregasi.
func (r *Retention) runAggregate() error {
	cutoff := time.Now().AddDate(0, 0, -r.retainRawDays)
	log.Printf("[INFO] retensi mode 'aggregate': meringkas data sebelum %s", cutoff.Format("2006-01-02"))

	// Pastikan tabel aggregasi mingguan ada
	if _, err := r.db.Exec(`
		CREATE TABLE IF NOT EXISTS folder_metrics_weekly (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			folder_path TEXT NOT NULL,
			depth_level INTEGER NOT NULL,
			week_start DATE NOT NULL,
			avg_size_bytes BIGINT NOT NULL,
			min_size_bytes BIGINT NOT NULL,
			max_size_bytes BIGINT NOT NULL,
			sample_count INTEGER NOT NULL,
			UNIQUE(folder_path, week_start)
		);
		CREATE INDEX IF NOT EXISTS idx_weekly_path ON folder_metrics_weekly(folder_path);
		CREATE INDEX IF NOT EXISTS idx_weekly_week ON folder_metrics_weekly(week_start);
	`); err != nil {
		return fmt.Errorf("gagal membuat tabel folder_metrics_weekly: %w", err)
	}

	// Agregasi: ringkas data mentah per folder per minggu
	_, err := r.db.Exec(`
		INSERT OR REPLACE INTO folder_metrics_weekly
			(folder_path, depth_level, week_start, avg_size_bytes, min_size_bytes, max_size_bytes, sample_count)
		SELECT
			folder_path,
			depth_level,
			date(start_date, 'weekday 0', '-6 days') AS week_start,
			CAST(AVG(size_bytes) AS BIGINT) AS avg_size_bytes,
			MIN(size_bytes) AS min_size_bytes,
			MAX(size_bytes) AS max_size_bytes,
			COUNT(*) AS sample_count
		FROM folder_metrics
		WHERE is_active = 0
		  AND end_date IS NOT NULL
		  AND end_date < ?
		GROUP BY folder_path, week_start
	`, cutoff)
	if err != nil {
		return fmt.Errorf("gagal menjalankan agregasi mingguan: %w", err)
	}

	// Hapus data mentah yang sudah diagregasi
	result, err := r.db.Exec(
		`DELETE FROM folder_metrics WHERE is_active = 0 AND end_date IS NOT NULL AND end_date < ?`,
		cutoff,
	)
	if err != nil {
		return fmt.Errorf("gagal menghapus data mentah setelah agregasi: %w", err)
	}

	rows, _ := result.RowsAffected()
	log.Printf("[INFO] retensi selesai: %d baris data mentah diagregasi dan dihapus", rows)

	if _, err := r.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		log.Printf("[WARN] gagal menjalankan incremental_vacuum setelah retensi: %v", err)
	}

	return nil
}
