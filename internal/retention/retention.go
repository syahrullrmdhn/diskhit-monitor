// Package retention controls bounded historical storage.
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

func (r *Retention) Run() error {
	cutoff := time.Now().AddDate(0, 0, -r.retainRawDays)
	tx, err := r.db.Begin()
	if err != nil {
		return fmt.Errorf("gagal membuka transaksi retensi: %w", err)
	}
	defer tx.Rollback()

	switch r.mode {
	case "delete":
		err = r.deleteRaw(tx, cutoff)
	case "aggregate":
		err = r.aggregateAndDelete(tx, cutoff)
	default:
		return fmt.Errorf("mode retensi tidak dikenal: %q", r.mode)
	}
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("gagal commit retensi: %w", err)
	}
	if _, err := r.db.Exec("PRAGMA incremental_vacuum"); err != nil {
		log.Printf("[WARN] gagal menjalankan incremental_vacuum setelah retensi: %v", err)
	}
	log.Printf("[INFO] retensi %s selesai untuk data sebelum %s", r.mode, cutoff.Format("2006-01-02"))
	return nil
}

func (r *Retention) deleteRaw(tx *sql.Tx, cutoff time.Time) error {
	queries := []string{
		`DELETE FROM folder_snapshots WHERE scan_date < ?`,
		`DELETE FROM scan_history WHERE scan_date < ?`,
		`DELETE FROM folder_metrics WHERE is_active = 0 AND end_date IS NOT NULL AND end_date < ?`,
	}
	for _, q := range queries {
		if _, err := tx.Exec(q, cutoff); err != nil {
			return fmt.Errorf("gagal menghapus data retensi: %w", err)
		}
	}
	return nil
}

func (r *Retention) aggregateAndDelete(tx *sql.Tx, cutoff time.Time) error {
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS folder_snapshots_weekly (
			folder_path TEXT NOT NULL,
			drive_letter TEXT NOT NULL,
			week_start DATE NOT NULL,
			avg_size_bytes BIGINT NOT NULL,
			min_size_bytes BIGINT NOT NULL,
			max_size_bytes BIGINT NOT NULL,
			sample_count INTEGER NOT NULL,
			PRIMARY KEY(folder_path, drive_letter, week_start)
		)`); err != nil {
		return fmt.Errorf("gagal membuat tabel agregat snapshot: %w", err)
	}

	// Merge newly eligible samples into an existing week before deleting raw
	// rows. Weighted average and sample count preserve every sample exactly
	// once as the rolling cutoff advances through that week.
	if _, err := tx.Exec(`
		INSERT INTO folder_snapshots_weekly
			(folder_path, drive_letter, week_start, avg_size_bytes, min_size_bytes, max_size_bytes, sample_count)
		SELECT folder_path, drive_letter,
		       date(substr(scan_date, 1, 10), 'weekday 0', '-6 days'),
		       CAST(AVG(size_bytes) AS BIGINT), MIN(size_bytes), MAX(size_bytes), COUNT(*)
		FROM folder_snapshots WHERE scan_date < ?
		GROUP BY folder_path, drive_letter, date(substr(scan_date, 1, 10), 'weekday 0', '-6 days')
		ON CONFLICT(folder_path, drive_letter, week_start) DO UPDATE SET
			avg_size_bytes = CAST((folder_snapshots_weekly.avg_size_bytes * folder_snapshots_weekly.sample_count + excluded.avg_size_bytes * excluded.sample_count) /
				(folder_snapshots_weekly.sample_count + excluded.sample_count) AS BIGINT),
			min_size_bytes = MIN(folder_snapshots_weekly.min_size_bytes, excluded.min_size_bytes),
			max_size_bytes = MAX(folder_snapshots_weekly.max_size_bytes, excluded.max_size_bytes),
			sample_count = folder_snapshots_weekly.sample_count + excluded.sample_count`, cutoff); err != nil {
		return fmt.Errorf("gagal mengagregasi snapshot mingguan: %w", err)
	}
	return r.deleteRaw(tx, cutoff)
}
