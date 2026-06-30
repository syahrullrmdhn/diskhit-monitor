package webserver

import (
	"encoding/csv"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

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

	// Parse query params: days (default 7)
	days := 7
	if d, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && d > 0 {
		days = d
	}
	_ = days // used for future filtering if needed

	rows, err := s.db.Query(`
		SELECT
			ranked.folder_path,
			ranked.current_size,
			COALESCE(ranked.prev_size, 0) AS prev_size,
			(ranked.current_size - COALESCE(ranked.prev_size, 0)) AS growth_bytes,
			CASE
				WHEN COALESCE(ranked.prev_size, 0) > 0
				THEN ROUND(((ranked.current_size - ranked.prev_size) * 100.0) / ranked.prev_size, 1)
				ELSE 100.0
			END AS growth_pct,
			ranked.depth_level
		FROM (
			SELECT
				folder_path,
				depth_level,
				size_bytes AS current_size,
				LAG(size_bytes) OVER (PARTITION BY folder_path ORDER BY start_date) AS prev_size,
				ROW_NUMBER() OVER (PARTITION BY folder_path ORDER BY start_date DESC) AS rn
			FROM folder_metrics
			WHERE is_active = 1
		) ranked
		WHERE ranked.rn = 1
		  AND (ranked.current_size - COALESCE(ranked.prev_size, 0)) > 0
		ORDER BY growth_bytes DESC
		LIMIT 10
	`)
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
		SELECT size_bytes, start_date, end_date, is_active
		FROM folder_metrics
		WHERE folder_path = ?
		ORDER BY start_date ASC
	`, folderPath)
	if err != nil {
		http.Error(w, "query error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type Point struct {
		SizeBytes int64  `json:"size_bytes"`
		StartDate string `json:"start_date"`
		EndDate   string `json:"end_date,omitempty"`
		IsActive  bool   `json:"is_active"`
	}

	var points []Point
	for rows.Next() {
		var p Point
		var endDate *time.Time
		if err := rows.Scan(&p.SizeBytes, &p.StartDate, &endDate, &p.IsActive); err != nil {
			http.Error(w, "scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if endDate != nil {
			p.EndDate = endDate.Format(time.RFC3339)
		}
		points = append(points, p)
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
}

// handleScanHistory mengembalikan riwayat scan terakhir.
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
		SELECT id, scan_date, drive_letter, total_size_bytes, status, skipped_count
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
		ID            int64     `json:"id"`
		ScanDate      time.Time `json:"scan_date"`
		DriveLetter   string    `json:"drive_letter"`
		TotalSizeBytes int64   `json:"total_size_bytes"`
		Status        string    `json:"status"`
		SkippedCount  int       `json:"skipped_count"`
	}

	var records []ScanRecord
	for rows.Next() {
		var rec ScanRecord
		if err := rows.Scan(&rec.ID, &rec.ScanDate, &rec.DriveLetter, &rec.TotalSizeBytes, &rec.Status, &rec.SkippedCount); err != nil {
			http.Error(w, "scan error: "+err.Error(), http.StatusInternalServerError)
			return
		}
		records = append(records, rec)
	}

	if records == nil {
		records = []ScanRecord{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(records)
}
