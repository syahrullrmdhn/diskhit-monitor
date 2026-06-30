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
		status TEXT NOT NULL CHECK (status IN ('success','partial','failed')),
		skipped_count INTEGER NOT NULL DEFAULT 0
	);

	CREATE TABLE IF NOT EXISTS folder_metrics (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		folder_path TEXT NOT NULL,
		depth_level INTEGER NOT NULL,
		size_bytes BIGINT NOT NULL,
		start_date DATETIME NOT NULL,
		end_date DATETIME,
		is_active BOOLEAN NOT NULL DEFAULT 1
	);

	CREATE INDEX IF NOT EXISTS idx_folder_metrics_path ON folder_metrics(folder_path);
	CREATE INDEX IF NOT EXISTS idx_folder_metrics_depth ON folder_metrics(depth_level);
	`
	_, err := s.db.Exec(schema)
	return err
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
