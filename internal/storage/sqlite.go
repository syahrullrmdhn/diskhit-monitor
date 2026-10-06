package storage

import (
	"database/sql"
	"fmt"
	"log"

	_ "modernc.org/sqlite" // pure Go SQLite driver, tidak butuh CGO/compiler C
)

// Store membungkus koneksi database dan menyediakan operasi terkait
// scan_history dan folder_metrics sesuai skema PRD/SRS Bagian 2.4.
type Store struct {
	db *sql.DB
}

// Open membuka (atau membuat) database SQLite di path yang diberikan,
// mengaktifkan WAL mode (wajib, agar web server bisa baca konkuren saat
// scanner menulis - lihat catatan implementasi Bagian 2.4), dan
// menjalankan migrasi schema otomatis jika tabel belum ada.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("gagal membuka database di %q: %w", path, err)
	}

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA auto_vacuum = INCREMENTAL;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return nil, fmt.Errorf("gagal menjalankan pragma %q pada database %q: %w", p, path, err)
		}
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("gagal migrasi schema database %q: %w", path, err)
	}

	log.Printf("[INFO] database siap dipakai di %q (WAL mode aktif)", path)
	return s, nil
}

// migrate membuat tabel scan_history dan folder_metrics jika belum ada.
// Sesuai Definisi Selesai MVP (Bagian 4.4 poin 5): tidak perlu tool migrasi
// eksternal terpisah untuk versi awal ini.
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS scan_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scan_date DATETIME NOT NULL,
		drive_letter TEXT NOT NULL,
		total_size_bytes BIGINT NOT NULL,
		total_allocated_bytes BIGINT NOT NULL DEFAULT 0,
		status TEXT NOT NULL CHECK (status IN ('success','partial','failed')),
		skipped_count INTEGER NOT NULL DEFAULT 0,
		consistency TEXT NOT NULL DEFAULT 'live'
		,finished_at DATETIME
	);

	CREATE TABLE IF NOT EXISTS folder_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		folder_path TEXT NOT NULL,
		depth_level INTEGER NOT NULL,
		size_bytes BIGINT NOT NULL,
		allocated_bytes BIGINT NOT NULL DEFAULT 0,
		start_date DATETIME NOT NULL,
		end_date DATETIME,
		is_active BOOLEAN NOT NULL DEFAULT 1
	);

	CREATE TABLE IF NOT EXISTS folder_snapshots_weekly (
		folder_path TEXT NOT NULL,
		drive_letter TEXT NOT NULL,
		week_start DATE NOT NULL,
		avg_size_bytes BIGINT NOT NULL,
		min_size_bytes BIGINT NOT NULL,
		max_size_bytes BIGINT NOT NULL,
		sample_count INTEGER NOT NULL,
		PRIMARY KEY(folder_path, drive_letter, week_start)
	);
	CREATE INDEX IF NOT EXISTS idx_snapshot_weekly_path ON folder_snapshots_weekly(folder_path);

	CREATE INDEX IF NOT EXISTS idx_folder_metrics_path ON folder_metrics(folder_path);
	CREATE INDEX IF NOT EXISTS idx_folder_metrics_depth ON folder_metrics(depth_level);

	-- Immutable per-scan snapshots are the source of truth for accurate
	-- day/week/month comparisons. The legacy differential table remains for
	-- backward compatibility with existing installations.
	CREATE TABLE IF NOT EXISTS folder_snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		scan_id INTEGER NOT NULL,
		scan_date DATETIME NOT NULL,
		drive_letter TEXT NOT NULL,
		folder_path TEXT NOT NULL,
		depth_level INTEGER NOT NULL,
		size_bytes BIGINT NOT NULL,
		allocated_bytes BIGINT NOT NULL DEFAULT 0,
		FOREIGN KEY(scan_id) REFERENCES scan_history(id) ON DELETE CASCADE
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_folder_snapshots_scan_path ON folder_snapshots(scan_id, folder_path);
	CREATE INDEX IF NOT EXISTS idx_folder_snapshots_path_date ON folder_snapshots(folder_path, scan_date);
	CREATE INDEX IF NOT EXISTS idx_folder_snapshots_date ON folder_snapshots(scan_date);

	CREATE TABLE IF NOT EXISTS scan_outbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		agent_id TEXT NOT NULL,
		drive_letter TEXT NOT NULL,
		scan_id INTEGER NOT NULL,
		idempotency_key TEXT NOT NULL UNIQUE,
		status TEXT NOT NULL CHECK (status IN ('pending','sending','sent','dead_letter')),
		attempts INTEGER NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		created_at DATETIME NOT NULL,
		updated_at DATETIME,
		lease_until DATETIME,
		sent_at DATETIME,
		FOREIGN KEY(scan_id) REFERENCES scan_history(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_scan_outbox_pending ON scan_outbox(status, created_at);
	`
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	additions := []struct{ table, column, definition string }{
		{"scan_history", "total_allocated_bytes", "BIGINT NOT NULL DEFAULT 0"},
		{"scan_history", "consistency", "TEXT NOT NULL DEFAULT 'live'"},
		{"folder_metrics", "allocated_bytes", "BIGINT NOT NULL DEFAULT 0"},
		{"folder_snapshots", "allocated_bytes", "BIGINT NOT NULL DEFAULT 0"},
		{"scan_history", "finished_at", "DATETIME"},
		{"scan_outbox", "lease_until", "DATETIME"},
	}
	for _, addition := range additions {
		exists, err := columnExists(s.db, addition.table, addition.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", addition.table, addition.column, addition.definition)); err != nil {
				return fmt.Errorf("add column %s.%s: %w", addition.table, addition.column, err)
			}
		}
	}
	return nil
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// Close menutup koneksi database.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB mengembalikan handle *sql.DB mentah untuk dipakai package lain
// (scanner, webserver, retention) yang butuh query custom.
func (s *Store) DB() *sql.DB {
	return s.db
}
