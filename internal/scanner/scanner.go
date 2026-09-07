package scanner

import (
	"context"
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
	Drive               string
	TotalSizeBytes      int64
	TotalAllocatedBytes int64
	SkippedCount        int
	Status              string // "success", "partial", atau "failed"
	Consistency         string // "vss", "live", atau "failed"
	ScanDate            time.Time
}

// Options controls accuracy and bounded scan resources.
type Options struct {
	UseVSS           bool
	CollectAllocated bool
	ScanTimeout      time.Duration
	CommandTimeout   time.Duration
}

// Scanner menjalankan recursive directory walk sesuai FR-01 (max depth)
// dan FR-08 (skip per-folder error, tidak pernah crash total).
type Scanner struct {
	db       *sql.DB
	maxDepth int
	options  Options
	provider SnapshotProvider
	running  atomic.Bool
}

func New(db *sql.DB, maxDepth int) *Scanner {
	return NewWithOptions(db, maxDepth, Options{ScanTimeout: 2 * time.Hour, CommandTimeout: 2 * time.Minute})
}

func NewWithOptions(db *sql.DB, maxDepth int, options Options) *Scanner {
	if options.ScanTimeout <= 0 {
		options.ScanTimeout = 2 * time.Hour
	}
	if options.CommandTimeout <= 0 {
		options.CommandTimeout = 2 * time.Minute
	}
	return &Scanner{db: db, maxDepth: maxDepth, options: options, provider: NewSnapshotProvider(options.CommandTimeout)}
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
	return s.ScanDriveContext(context.Background(), drive)
}

// ScanDriveContext allows service shutdown to cancel traversal and wait for
// VSS cleanup before the process exits.
func (s *Scanner) ScanDriveContext(parent context.Context, drive string) Result {
	start := time.Now()
	result := Result{Drive: drive, Status: "failed", Consistency: "failed", ScanDate: start}
	if !s.running.CompareAndSwap(false, true) {
		log.Printf("[WARN] scan drive %q dilewati karena scan lain masih berjalan", drive)
		return result
	}
	defer s.running.Store(false)

	ctx, cancel := context.WithTimeout(parent, s.options.ScanTimeout)
	defer cancel()
	snapshot := Snapshot{SourceRoot: drive, ReportedRoot: drive, Consistent: false}
	if s.options.UseVSS {
		var err error
		snapshot, err = s.provider.Create(ctx, drive)
		if err != nil {
			log.Printf("[ERROR] VSS snapshot gagal untuk %q: %v", drive, err)
			s.persistFailedScan(result)
			return result
		}
		// Cleanup must not inherit an expired scan context.
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), s.options.CommandTimeout)
			defer cleanupCancel()
			if err := s.provider.Delete(cleanupCtx, snapshot); err != nil {
				log.Printf("[ERROR] cleanup VSS snapshot %q gagal: %v", snapshot.ID, err)
			}
		}()
	}

	metrics, totals, skipped := collectFolderMetricsContext(ctx, snapshot.SourceRoot, drive, s.maxDepth, s.options.CollectAllocated)
	if ctx.Err() != nil {
		log.Printf("[ERROR] scan drive %q melewati timeout %s", drive, s.options.ScanTimeout)
		s.persistFailedScan(result)
		return result
	}
	result.TotalSizeBytes = totals.Logical
	result.TotalAllocatedBytes = totals.Allocated
	result.SkippedCount = skipped
	result.Status = "success"
	result.Consistency = "live"
	if snapshot.Consistent {
		result.Consistency = "vss"
	}
	if skipped > 0 {
		result.Status = "partial"
	}
	if err := s.persistMetrics(drive, metrics, result); err != nil {
		log.Printf("[ERROR] gagal menyimpan hasil scan drive %q ke database: %v", drive, err)
		result.Status = "failed"
		result.Consistency = "failed"
		s.persistFailedScan(result)
	}
	return result
}

// folderMetric stores logical EOF bytes and allocated on-disk bytes.
type folderMetric struct {
	Logical   int64
	Allocated int64
}

// collectFolderSizes is retained for compatibility with logical-size tests.
func collectFolderSizes(root string, maxDepth int) (map[string]int64, int64, int) {
	metrics, total, skipped := collectFolderMetrics(root, root, maxDepth, false)
	sizes := make(map[string]int64, len(metrics))
	for path, metric := range metrics {
		sizes[path] = metric.Logical
	}
	return sizes, total.Logical, skipped
}

// collectFolderMetrics walks sourceRoot but stores only paths under reportedRoot.
func collectFolderMetrics(sourceRoot, reportedRoot string, maxDepth int, collectAllocated bool) (map[string]folderMetric, folderMetric, int) {
	return collectFolderMetricsContext(context.Background(), sourceRoot, reportedRoot, maxDepth, collectAllocated)
}

func collectFolderMetricsContext(ctx context.Context, sourceRoot, reportedRoot string, maxDepth int, collectAllocated bool) (map[string]folderMetric, folderMetric, int) {
	sourceRoot = filepath.Clean(sourceRoot)
	reportedRoot = filepath.Clean(reportedRoot)
	metrics := map[string]folderMetric{reportedRoot: {}}
	var total folderMetric
	var skipped int

	err := filepath.WalkDir(sourceRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			skipped++
			log.Printf("[WARN] skip path %q karena error: %v", path, walkErr)
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == sourceRoot {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || isReparsePoint(path, d) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(sourceRoot, path)
		if relErr != nil {
			skipped++
			return nil
		}
		reportedPath := filepath.Join(reportedRoot, rel)
		if d.IsDir() {
			if depthOf(reportedRoot, reportedPath) <= maxDepth {
				metrics[filepath.Clean(reportedPath)] = folderMetric{}
			}
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			skipped++
			return nil
		}
		logical := info.Size()
		allocated := logical
		if collectAllocated {
			allocated, infoErr = allocatedFileSize(path, logical)
			if infoErr != nil {
				skipped++
				log.Printf("[WARN] allocated size gagal untuk %q: %v", path, infoErr)
				return nil
			}
		}
		total.Logical += logical
		total.Allocated += allocated
		dir := filepath.Dir(reportedPath)
		for {
			depth := depthOf(reportedRoot, dir)
			if depth <= maxDepth {
				metric := metrics[filepath.Clean(dir)]
				metric.Logical += logical
				metric.Allocated += allocated
				metrics[filepath.Clean(dir)] = metric
			}
			if filepath.Clean(dir) == reportedRoot {
				break
			}
			parent := filepath.Dir(dir)
			if parent == dir || !isWithinRoot(reportedRoot, parent) {
				break
			}
			dir = parent
		}
		return nil
	})
	if err != nil {
		skipped++
		log.Printf("[ERROR] scan root %q gagal: %v", sourceRoot, err)
	}
	return metrics, total, skipped
}

func isWithinRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
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
	metrics := make(map[string]folderMetric, len(sizes))
	for path, size := range sizes {
		metrics[path] = folderMetric{Logical: size, Allocated: size}
	}
	return s.persistMetrics(drive, metrics, result)
}

func (s *Scanner) persistMetrics(drive string, sizes map[string]folderMetric, result Result) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("gagal membuka transaksi: %w", err)
	}
	defer tx.Rollback()

	now := result.ScanDate
	seenPaths := make(map[string]bool)

	for path, metric := range sizes {
		size := metric.Logical
		allocated := metric.Allocated
		seenPaths[path] = true

		var existingID int64
		var existingSize int64
		var existingAllocated int64
		err := tx.QueryRow(
			`SELECT id, size_bytes, allocated_bytes FROM folder_metrics WHERE folder_path = ? AND is_active = 1 ORDER BY start_date DESC LIMIT 1`,
			path,
		).Scan(&existingID, &existingSize, &existingAllocated)

		switch {
		case err == sql.ErrNoRows:
			// Belum pernah tercatat - insert baru.
			_, err = tx.Exec(
				`INSERT INTO folder_metrics (folder_path, depth_level, size_bytes, allocated_bytes, start_date, is_active)
				 VALUES (?, ?, ?, ?, ?, 1)`,
				path, depthOf(drive, path), size, allocated, now,
			)
			if err != nil {
				return fmt.Errorf("gagal insert folder_metrics baru untuk %q: %w", path, err)
			}
		case err != nil:
			return fmt.Errorf("gagal query folder_metrics untuk %q: %w", path, err)
		case existingSize == size && existingAllocated == allocated:
			// Kedua metrik sama: record aktif tetap terbuka. Snapshot terpisah tetap
			// mencatat observasi scan ini untuk analitik berbasis waktu.
			continue
		default:
			// Ukuran berubah - segel record lama, insert baris baru.
			_, err = tx.Exec(`UPDATE folder_metrics SET end_date = ?, is_active = 0 WHERE id = ?`, now, existingID)
			if err != nil {
				return fmt.Errorf("gagal menyegel record lama untuk %q: %w", path, err)
			}
			_, err = tx.Exec(
				`INSERT INTO folder_metrics (folder_path, depth_level, size_bytes, allocated_bytes, start_date, is_active)
				 VALUES (?, ?, ?, ?, ?, 1)`,
				path, depthOf(drive, path), size, allocated, now,
			)
			if err != nil {
				return fmt.Errorf("gagal insert record baru (size berubah) untuk %q: %w", path, err)
			}
		}
	}

	// Tandai folder yang dulu aktif tapi sekarang tidak ditemukan lagi sebagai
	// tidak aktif (folder terhapus dari disk) - lihat Bagian 2.4.
	rows, err := tx.Query(`SELECT id, folder_path FROM folder_metrics WHERE is_active = 1`)
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
		// A partial scan cannot prove absence: a permission/read error may hide
		// an entire subtree. Only mark deletions after a complete scan.
		if result.Status == "success" && !seenPaths[path] && isWithinRoot(drive, path) {
			toDeactivate = append(toDeactivate, id)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("gagal iterasi folder aktif: %w", err)
	}
	rows.Close()
	for _, id := range toDeactivate {
		_, err = tx.Exec(`UPDATE folder_metrics SET end_date = ?, is_active = 0 WHERE id = ?`, now, id)
		if err != nil {
			return fmt.Errorf("gagal menandai folder terhapus (id=%d): %w", id, err)
		}
	}

	res, err := tx.Exec(
		`INSERT INTO scan_history (scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		now, drive, result.TotalSizeBytes, result.TotalAllocatedBytes, result.Status, result.SkippedCount, result.Consistency,
	)
	if err != nil {
		return fmt.Errorf("gagal insert scan_history: %w", err)
	}
	scanID, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("gagal membaca id scan_history: %w", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO folder_snapshots
		(scan_id, scan_date, drive_letter, folder_path, depth_level, size_bytes, allocated_bytes)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("gagal menyiapkan snapshot folder: %w", err)
	}
	defer stmt.Close()
	for path, metric := range sizes {
		if _, err := stmt.Exec(scanID, now, drive, path, depthOf(drive, path), metric.Logical, metric.Allocated); err != nil {
			return fmt.Errorf("gagal menyimpan snapshot folder %q: %w", path, err)
		}
	}

	return tx.Commit()
}

func (s *Scanner) persistFailedScan(result Result) {
	if _, err := s.db.Exec(`INSERT INTO scan_history
		(scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency)
		VALUES (?, ?, 0, 0, 'failed', ?, 'failed')`, result.ScanDate, result.Drive, result.SkippedCount); err != nil {
		log.Printf("[ERROR] gagal mencatat scan gagal untuk %q: %v", result.Drive, err)
	}
}

func depthOf(drive, path string) int {
	rel, err := filepath.Rel(drive, path)
	if err != nil {
		return 1
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}
