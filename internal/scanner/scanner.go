package scanner

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// Result merangkum hasil satu kali eksekusi scan, dipakai untuk
// mengisi tabel scan_history dan endpoint /healthz (NFR-07).
type Result struct {
	Drive          string
	TotalSizeBytes int64
	SkippedCount   int
	Status         string // "success", "partial", atau "failed"
	ScanDate       time.Time
}

// Scanner menjalankan recursive directory walk sesuai FR-01 (max depth)
// dan FR-08 (skip per-folder error, tidak pernah crash total).
type Scanner struct {
	db       *sql.DB
	maxDepth int
}

func New(db *sql.DB, maxDepth int) *Scanner {
	return &Scanner{db: db, maxDepth: maxDepth}
}

// folderSize menyimpan akumulasi ukuran tiap folder pada level <= maxDepth.
type folderSize struct {
	path string
	size int64
}

// ScanDrive memindai satu drive (misal "D:\\") dan menyimpan hasilnya
// ke database. Setiap error per-file/folder di-log sebagai WARN dan
// di-skip, TIDAK menghentikan seluruh proses (FR-08, NFR-04).
//
// Seluruh fungsi ini dibungkus recover() di pemanggil (lihat main.go)
// sesuai NFR-06, sehingga panic tak terduga pun tidak menjatuhkan
// proses web server yang berjalan di goroutine lain.
func (s *Scanner) ScanDrive(drive string) Result {
	start := time.Now()
	log.Printf("[INFO] mulai scan drive %q (max_depth=%d)", drive, s.maxDepth)

	sizes := make(map[string]int64) // path -> ukuran terakumulasi
	var skippedCount int64
	var totalSize int64

	walkErr := filepath.WalkDir(drive, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// FR-08: jangan return error ke WalkDir (itu akan menghentikan walk).
			// Log sebagai WARN dengan detail spesifik (NFR-08), lalu skip.
			atomic.AddInt64(&skippedCount, 1)
			log.Printf("[WARN] skip path %q karena error: %v (kemungkinan permission denied, reparse point rusak, atau path terlalu panjang)", path, err)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			atomic.AddInt64(&skippedCount, 1)
			log.Printf("[WARN] skip file %q, gagal membaca info ukuran: %v", path, err)
			return nil
		}

		size := info.Size()
		atomic.AddInt64(&totalSize, size)

		// Hitung folder induk pada level <= maxDepth (FR-01).
		bucket := bucketAtDepth(drive, path, s.maxDepth)
		sizes[bucket] += size

		return nil
	})

	status := "success"
	if walkErr != nil {
		// walkErr di sini seharusnya jarang terjadi karena handler di atas
		// sudah menangani error per-item. Tapi tetap di-log spesifik jika muncul.
		status = "failed"
		log.Printf("[ERROR] scan drive %q gagal total: %v", drive, walkErr)
	} else if skippedCount > 0 {
		status = "partial"
	}

	result := Result{
		Drive:          drive,
		TotalSizeBytes: totalSize,
		SkippedCount:   int(skippedCount),
		Status:         status,
		ScanDate:       start,
	}

	if err := s.persist(drive, sizes, result); err != nil {
		// Kegagalan menulis ke DB itu serius (NFR-08: ERROR level),
		// tapi tetap tidak panic - proses lain (web server) harus tetap hidup.
		log.Printf("[ERROR] gagal menyimpan hasil scan drive %q ke database: %v", drive, err)
		result.Status = "failed"
	}

	log.Printf("[INFO] selesai scan drive %q dalam %s, total %d bytes, %d folder di-skip, status=%s",
		drive, time.Since(start), totalSize, skippedCount, result.Status)

	return result
}

// bucketAtDepth menentukan folder "induk" pada level depth maksimum.
// Semua ukuran subfolder di bawah level ini digabung ke folder induk
// pada level tersebut, sesuai FR-01.
func bucketAtDepth(drive, fullPath string, maxDepth int) string {
	rel, err := filepath.Rel(drive, fullPath)
	if err != nil {
		return filepath.Dir(fullPath)
	}
	parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	if len(parts) > maxDepth {
		parts = parts[:maxDepth]
	}
	return filepath.Join(drive, filepath.Join(parts...))
}

// persist menerapkan Differential Logging sesuai FR-02 dan Bagian 2.4:
// hanya insert baris baru jika ukuran berubah dibanding record aktif
// sebelumnya; jika sama, cukup update end_date.
func (s *Scanner) persist(drive string, sizes map[string]int64, result Result) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("gagal membuka transaksi: %w", err)
	}
	defer tx.Rollback()

	now := result.ScanDate
	seenPaths := make(map[string]bool)

	for path, size := range sizes {
		seenPaths[path] = true

		var existingID int64
		var existingSize int64
		err := tx.QueryRow(
			`SELECT id, size_bytes FROM folder_metrics WHERE folder_path = ? AND is_active = 1 AND end_date IS NULL`,
			path,
		).Scan(&existingID, &existingSize)

		switch {
		case err == sql.ErrNoRows:
			// Belum pernah tercatat - insert baru.
			_, err = tx.Exec(
				`INSERT INTO folder_metrics (folder_path, depth_level, size_bytes, start_date, is_active)
				 VALUES (?, ?, ?, ?, 1)`,
				path, depthOf(drive, path), size, now,
			)
			if err != nil {
				return fmt.Errorf("gagal insert folder_metrics baru untuk %q: %w", path, err)
			}
		case err != nil:
			return fmt.Errorf("gagal query folder_metrics untuk %q: %w", path, err)
		case existingSize == size:
			// Ukuran sama - cukup update end_date (FR-02: hemat ruang).
			_, err = tx.Exec(`UPDATE folder_metrics SET end_date = ? WHERE id = ?`, now, existingID)
			if err != nil {
				return fmt.Errorf("gagal update end_date untuk %q: %w", path, err)
			}
		default:
			// Ukuran berubah - segel record lama, insert baris baru.
			_, err = tx.Exec(`UPDATE folder_metrics SET end_date = ? WHERE id = ?`, now, existingID)
			if err != nil {
				return fmt.Errorf("gagal menyegel record lama untuk %q: %w", path, err)
			}
			_, err = tx.Exec(
				`INSERT INTO folder_metrics (folder_path, depth_level, size_bytes, start_date, is_active)
				 VALUES (?, ?, ?, ?, 1)`,
				path, depthOf(drive, path), size, now,
			)
			if err != nil {
				return fmt.Errorf("gagal insert record baru (size berubah) untuk %q: %w", path, err)
			}
		}
	}

	// Tandai folder yang dulu aktif tapi sekarang tidak ditemukan lagi sebagai
	// tidak aktif (folder terhapus dari disk) - lihat Bagian 2.4.
	rows, err := tx.Query(`SELECT id, folder_path FROM folder_metrics WHERE is_active = 1 AND end_date IS NULL`)
	if err != nil {
		return fmt.Errorf("gagal query folder aktif untuk deteksi penghapusan: %w", err)
	}
	var toDeactivate []int64
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			rows.Close()
			return fmt.Errorf("gagal scan baris folder aktif: %w", err)
		}
		if !seenPaths[path] && strings.HasPrefix(path, drive) {
			toDeactivate = append(toDeactivate, id)
		}
	}
	rows.Close()

	for _, id := range toDeactivate {
		_, err = tx.Exec(`UPDATE folder_metrics SET end_date = ?, is_active = 0 WHERE id = ?`, now, id)
		if err != nil {
			return fmt.Errorf("gagal menandai folder terhapus (id=%d): %w", id, err)
		}
	}

	_, err = tx.Exec(
		`INSERT INTO scan_history (scan_date, drive_letter, total_size_bytes, status, skipped_count)
		 VALUES (?, ?, ?, ?, ?)`,
		now, drive, result.TotalSizeBytes, result.Status, result.SkippedCount,
	)
	if err != nil {
		return fmt.Errorf("gagal insert scan_history: %w", err)
	}

	return tx.Commit()
}

func depthOf(drive, path string) int {
	rel, err := filepath.Rel(drive, path)
	if err != nil {
		return 1
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}
