// Package report mengirim hasil scan DiskHist ke server Ndelik melalui
// endpoint agent POST /agent/v1/storage (lihat kontrak A10).
package report

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	storagePath    = "/agent/v1/storage"
	pushTimeout    = 5 * time.Minute
	maxBodyBytes   = 64 << 20
	maxAckBytes    = 1 << 20
	payloadVersion = "1"
)

// Client mengirim payload scan ke satu server Ndelik.
type Client struct {
	Server string
	APIKey string
	HC     *http.Client
	Retry  RetryPolicy
}

// RetryPolicy controls retries; Sleep is injectable for deterministic tests.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	Sleep          func(context.Context, time.Duration) error
}

// NewClient membuat client dengan HTTP timeout 5 menit.
func NewClient(server, apiKey string) *Client {
	return &Client{Server: server, APIKey: apiKey, HC: &http.Client{Timeout: pushTimeout}}
}

// ScanJSON adalah metadata satu scan dalam bentuk yang diterima Ndelik.
type ScanJSON struct {
	Drive               string     `json:"drive"`
	StartedAt           time.Time  `json:"started_at"`
	FinishedAt          *time.Time `json:"finished_at"`
	Status              string     `json:"status"`
	Consistency         string     `json:"consistency"`
	VSSMode             string     `json:"vss_mode"`
	Error               string     `json:"error"`
	TotalBytes          int64      `json:"total_bytes"`
	TotalAllocatedBytes *int64     `json:"total_allocated_bytes"`
	FolderCount         int        `json:"folder_count"`
	SkippedCount        int        `json:"skipped_count"`
}

// FolderJSON adalah ukuran satu folder dalam bentuk yang diterima Ndelik.
type FolderJSON struct {
	Path           string `json:"path"`
	Depth          int    `json:"depth"`
	SizeBytes      int64  `json:"size_bytes"`
	AllocatedBytes *int64 `json:"allocated_bytes"`
	FileCount      *int   `json:"file_count"`
}

// Payload adalah isi satu kali POST ke /agent/v1/storage.
type Payload struct {
	Scan           ScanJSON     `json:"scan"`
	Folders        []FolderJSON `json:"folders"`
	Version        string       `json:"version"`
	IdempotencyKey string       `json:"idempotency_key"`
}

// Ack adalah respons sukses dari server Ndelik.
type Ack struct {
	ScanID   string `json:"scan_id"`
	Accepted int    `json:"accepted"`
	Rejected []struct {
		Path   string `json:"path"`
		Reason string `json:"reason"`
	} `json:"rejected"`
}

// Push mengirim satu payload scan dan mengembalikan ack server.
func (c *Client) Push(ctx context.Context, p Payload) (Ack, error) {
	u, err := url.Parse(strings.TrimRight(c.Server, "/") + storagePath)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return Ack{}, fmt.Errorf("report server must use HTTPS")
	}
	if p.IdempotencyKey == "" {
		p.IdempotencyKey = idempotencyKey(p)
	}
	body, err := json.Marshal(p)
	if err != nil {
		return Ack{}, fmt.Errorf("encode payload report: %w", err)
	}
	if len(body) > maxBodyBytes {
		return Ack{}, fmt.Errorf("payload report %d byte melebihi batas %d byte; kurangi max_depth atau drive", len(body), maxBodyBytes)
	}

	hc := c.HC
	if hc == nil {
		hc = &http.Client{Timeout: pushTimeout}
	}
	max := c.Retry.MaxAttempts
	if max < 1 {
		max = 1
	}
	backoff := c.Retry.InitialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	sleep := c.Retry.Sleep
	if sleep == nil {
		sleep = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}
	for attempt := 1; attempt <= max; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if e != nil {
			return Ack{}, e
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		req.Header.Set("Idempotency-Key", p.IdempotencyKey)
		resp, e := hc.Do(req)
		if e != nil {
			if attempt == max {
				return Ack{}, fmt.Errorf("kirim report gagal: %w", e)
			}
			if e = sleep(ctx, backoff); e != nil {
				return Ack{}, e
			}
			backoff *= 2
			continue
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAckBytes+1))
		resp.Body.Close()
		if readErr != nil {
			return Ack{}, fmt.Errorf("baca respons report: %w", readErr)
		}
		if len(respBody) > maxAckBytes {
			return Ack{}, fmt.Errorf("ACK response too large")
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) && attempt < max {
				if e = sleep(ctx, backoff); e != nil {
					return Ack{}, e
				}
				backoff *= 2
				continue
			}
			return Ack{}, fmt.Errorf("server menolak report (%s): %s", resp.Status, strings.TrimSpace(string(respBody)))
		}
		var ack Ack
		if err := json.Unmarshal(respBody, &ack); err != nil || ack.ScanID == "" || ack.Accepted < 0 || ack.Rejected == nil {
			return Ack{}, fmt.Errorf("invalid ACK response")
		}
		return ack, nil
	}
	return Ack{}, fmt.Errorf("report retry exhausted")
}

func idempotencyKey(p Payload) string {
	b, _ := json.Marshal(struct {
		S ScanJSON
		V string
	}{p.Scan, p.Version})
	return fmt.Sprintf("diskhist-%x", sha256.Sum256(b))
}

// LoadLatest membaca scan selesai (success/partial) terbaru untuk `drive`
// beserta baris folder_snapshots-nya dan menyusun Payload untuk Ndelik.
// Scan berstatus failed tidak punya snapshot folder, jadi dilewati.
func LoadLatest(ctx context.Context, db *sql.DB, drive string) (Payload, error) {
	var scanID int64
	if err := db.QueryRowContext(ctx, `SELECT id FROM scan_history WHERE drive_letter = ? AND status IN ('success','partial') ORDER BY scan_date DESC,id DESC LIMIT 1`, drive).Scan(&scanID); err != nil {
		return Payload{}, fmt.Errorf("belum ada scan selesai untuk drive %q di database DiskHist", drive)
	}
	return LoadScanByID(ctx, db, scanID)
}

func LoadScanByID(ctx context.Context, db *sql.DB, scanID int64) (Payload, error) {
	var (
		drive       string
		scanDate    time.Time
		totalSize   int64
		totalAlloc  int64
		status      string
		skipped     int
		consistency string
	)
	err := db.QueryRowContext(ctx, `
		SELECT drive_letter, scan_date, finished_at, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency
		FROM scan_history WHERE id = ? AND status IN ('success','partial')`, scanID).
		Scan(&drive, &scanDate, &finishedAt, &totalSize, &totalAlloc, &status, &skipped, &consistency)
	if err == sql.ErrNoRows {
		return Payload{}, fmt.Errorf("belum ada scan selesai id %d di database DiskHist", scanID)
	}
	if err != nil {
		return Payload{}, fmt.Errorf("query scan %d: %w", scanID, err)
	}
	folders, err := loadFolders(ctx, db, scanID)
	if err != nil {
		return Payload{}, fmt.Errorf("query folder scan %d (drive %q): %w", scanID, drive, err)
	}
	alloc := totalAlloc
	var finished *time.Time
	if finishedAt.Valid {
		finished = &finishedAt.Time
	}
	return Payload{
		Scan: ScanJSON{
			Drive:               drive,
			StartedAt:           scanDate,
			FinishedAt:          finished,
			Status:              mapStatus(status),
			Consistency:         mapConsistency(status),
			VSSMode:             mapVSSMode(consistency),
			Error:               "",
			TotalBytes:          totalSize,
			TotalAllocatedBytes: &alloc,
			FolderCount:         len(folders),
			SkippedCount:        skipped,
		},
		Folders: folders,
		Version: payloadVersion,
	}, nil
}

func loadFolders(ctx context.Context, db *sql.DB, scanID int64) ([]FolderJSON, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT folder_path, depth_level, size_bytes, allocated_bytes
		FROM folder_snapshots
		WHERE scan_id = ?
		ORDER BY size_bytes DESC, folder_path ASC`, scanID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	folders := make([]FolderJSON, 0, 64)
	for rows.Next() {
		var (
			path  string
			depth int
			size  int64
			alloc int64
		)
		if err := rows.Scan(&path, &depth, &size, &alloc); err != nil {
			return nil, err
		}
		a := alloc
		folders = append(folders, FolderJSON{
			Path:           path,
			Depth:          depth,
			SizeBytes:      size,
			AllocatedBytes: &a,
			FileCount:      nil,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return folders, nil
}

// mapStatus memetakan status DiskHist (success/partial/failed) ke status Ndelik
// (ok/partial/failed).
func mapStatus(status string) string {
	switch status {
	case "success":
		return "ok"
	case "partial":
		return "partial"
	default:
		return "failed"
	}
}

// mapConsistency: scan lengkap -> "exact"; selain itu "partial" agar hasil tidak
// pernah dilabeli lengkap saat ada path yang dilewati (kontrak A10).
func mapConsistency(status string) string {
	if status == "success" {
		return "exact"
	}
	return "partial"
}

// mapVSSMode memakai kosakata Ndelik (required/off) berdasarkan apakah snapshot
// point-in-time VSS benar-benar dipakai untuk scan tersebut.
func mapVSSMode(consistency string) string {
	if consistency == "vss" {
		return "required"
	}
	return "off"
}
