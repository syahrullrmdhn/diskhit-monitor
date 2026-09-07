package webserver

import (
	"encoding/csv"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

func parsePeriodDays(period string) int {
	switch period {
	case "day":
		return 1
	case "month":
		return 30
	default:
		return 7
	}
}

// handleHealthz mengimplementasikan FR-09/NFR-07: verifikasi proses sehat
// secara konkret, bukan cuma asumsi dari status Windows Service.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	uptime, lastScan, status, skipped := s.status.Snapshot()

	resp := map[string]interface{}{
		"uptime_seconds":   int(uptime.Seconds()),
		"last_scan_time":   lastScan,
		"last_scan_status": status,
		"skipped_count":    skipped,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleTopGrowers mengimplementasikan FR-03: Top 10 folder dengan
// pertumbuhan terbesar dalam rentang waktu tertentu.
func (s *Server) handleTopGrowers(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusInternalServerError)
		return
	}

	period := r.URL.Query().Get("period")
	days := parsePeriodDays(period)
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		days = d
	}
	periodOffset := "-" + strconv.Itoa(days) + " days"
	rows, err := s.db.Query(`
		WITH latest_scan AS (
			SELECT sh.id, sh.drive_letter, sh.scan_date FROM scan_history sh
			WHERE sh.status='success' AND sh.id=(
				SELECT s2.id FROM scan_history s2
				WHERE s2.drive_letter=sh.drive_letter AND s2.status='success'
				ORDER BY s2.scan_date DESC LIMIT 1)
		), current_snap AS (
			SELECT fs.* FROM folder_snapshots fs JOIN latest_scan x ON x.id=fs.scan_id
		), historical AS (
			SELECT fs.folder_path, fs.drive_letter, fs.size_bytes, fs.scan_date AS sample_date
			FROM folder_snapshots fs JOIN scan_history sh ON sh.id=fs.scan_id
			WHERE sh.status='success'
			UNION ALL
			SELECT folder_path, drive_letter, avg_size_bytes, week_start AS sample_date
			FROM folder_snapshots_weekly
		), baseline_ranked AS (
			SELECT h.*, ROW_NUMBER() OVER (
				PARTITION BY h.folder_path, h.drive_letter ORDER BY h.sample_date DESC
			) AS rn
			FROM historical h JOIN latest_scan l ON l.drive_letter=h.drive_letter
			WHERE h.sample_date <= strftime('%Y-%m-%dT%H:%M:%SZ', replace(substr(l.scan_date,1,19),'T',' '), ?)
		), baseline_snap AS (
			SELECT * FROM baseline_ranked WHERE rn=1
		)
		SELECT c.folder_path, c.size_bytes,
		       COALESCE(b.size_bytes, 0),
		       c.size_bytes - COALESCE(b.size_bytes, 0),
		       CASE WHEN COALESCE(b.size_bytes, 0) > 0
		            THEN ROUND((c.size_bytes - b.size_bytes) * 100.0 / b.size_bytes, 2)
		            WHEN c.size_bytes > 0 THEN 100.0 ELSE 0 END,
		       c.depth_level
		FROM current_snap c
		LEFT JOIN baseline_snap b ON b.folder_path = c.folder_path AND b.drive_letter = c.drive_letter
		WHERE c.size_bytes > COALESCE(b.size_bytes, c.size_bytes)
		ORDER BY (c.size_bytes - COALESCE(b.size_bytes, 0)) DESC
		LIMIT 10
	`, periodOffset)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Grower struct {
		Path        string  `json:"path"`
		CurrentSize int64   `json:"current_size"`
		PrevSize    int64   `json:"previous_size"`
		GrowthBytes int64   `json:"growth_bytes"`
		GrowthPct   float64 `json:"growth_pct"`
		Depth       int     `json:"depth"`
	}

	var growers []Grower
	for rows.Next() {
		var g Grower
		if err := rows.Scan(&g.Path, &g.CurrentSize, &g.PrevSize, &g.GrowthBytes, &g.GrowthPct, &g.Depth); err != nil {
			http.Error(w, "scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		growers = append(growers, g)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iteration error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if growers == nil {
		growers = []Grower{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(growers)
}

// handleFolderHistory mengimplementasikan FR-04: grafik historis ukuran folder.
func (s *Server) handleFolderHistory(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusInternalServerError)
		return
	}

	folderPath := r.URL.Query().Get("path")
	if folderPath == "" {
		http.Error(w, "query param 'path' wajib diisi", http.StatusBadRequest)
		return
	}

	rows, err := s.db.Query(`
		SELECT size_bytes, scan_date FROM (
			SELECT size_bytes, scan_date FROM folder_snapshots WHERE folder_path = ?
			UNION ALL
			SELECT avg_size_bytes, week_start FROM folder_snapshots_weekly WHERE folder_path = ?
		) ORDER BY scan_date ASC
	`, folderPath, folderPath)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Point struct {
		SizeBytes int64     `json:"size_bytes"`
		ScanDate  time.Time `json:"scan_date"`
		StartDate time.Time `json:"start_date"`
		EndDate   string    `json:"end_date,omitempty"`
		IsActive  bool      `json:"is_active"`
	}

	var points []Point
	for rows.Next() {
		var p Point
		if err := rows.Scan(&p.SizeBytes, &p.ScanDate); err != nil {
			http.Error(w, "scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		p.StartDate = p.ScanDate
		p.IsActive = true
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iteration error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if points == nil {
		points = []Point{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(points)
}

// handleExportCSV mengimplementasikan FR-06: ekspor data ke CSV.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusInternalServerError)
		return
	}

	days := 30
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		days = d
	}

	since := time.Now().AddDate(0, 0, -days)

	rows, err := s.db.Query(`
		SELECT folder_path, size_bytes, start_date, end_date, depth_level, is_active
		FROM folder_metrics
		WHERE start_date >= ? OR (end_date IS NOT NULL AND end_date >= ?)
		ORDER BY folder_path, start_date
	`, since, since)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=diskhist_export.csv")

	cw := csv.NewWriter(w)
	defer cw.Flush()

	cw.Write([]string{"folder_path", "size_bytes", "start_date", "end_date", "depth_level", "is_active"})

	for rows.Next() {
		var path string
		var size int64
		var startDate time.Time
		var endDate *time.Time
		var depth int
		var isActive bool

		if err := rows.Scan(&path, &size, &startDate, &endDate, &depth, &isActive); err != nil {
			log.Printf("[WARN] skip row di CSV export: %v", err)
			continue
		}

		endDateStr := ""
		if endDate != nil {
			endDateStr = endDate.Format("2006-01-02 15:04:05")
		}

		cw.Write([]string{
			path,
			strconv.FormatInt(size, 10),
			startDate.Format("2006-01-02 15:04:05"),
			endDateStr,
			strconv.Itoa(depth),
			strconv.FormatBool(isActive),
		})
	}
	if err := rows.Err(); err != nil {
		log.Printf("[ERROR] iterasi CSV gagal: %v", err)
	}
	if err := cw.Error(); err != nil {
		log.Printf("[ERROR] penulisan CSV gagal: %v", err)
	}
}
func (s *Server) handleScanHistory(w http.ResponseWriter, r *http.Request) {
	if s.db == nil {
		http.Error(w, "database not available", http.StatusInternalServerError)
		return
	}

	limit := 20
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 {
		limit = l
	}

	rows, err := s.db.Query(`
		SELECT id, scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency
		FROM scan_history
		ORDER BY scan_date DESC
		LIMIT ?
	`, limit)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type ScanRecord struct {
		ID                  int64     `json:"id"`
		ScanDate            time.Time `json:"scan_date"`
		DriveLetter         string    `json:"drive_letter"`
		TotalSizeBytes      int64     `json:"total_size_bytes"`
		TotalAllocatedBytes int64     `json:"total_allocated_bytes"`
		Status              string    `json:"status"`
		SkippedCount        int       `json:"skipped_count"`
		Consistency         string    `json:"consistency"`
	}

	var records []ScanRecord
	for rows.Next() {
		var rec ScanRecord
		if err := rows.Scan(&rec.ID, &rec.ScanDate, &rec.DriveLetter, &rec.TotalSizeBytes, &rec.TotalAllocatedBytes, &rec.Status, &rec.SkippedCount, &rec.Consistency); err != nil {
			http.Error(w, "scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		records = append(records, rec)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "iteration error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if records == nil {
		records = []ScanRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}
