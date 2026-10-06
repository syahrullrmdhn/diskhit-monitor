package report

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diskhist-monitor/internal/storage"
)

func openTestDB(t *testing.T) *storage.Store {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatalf("buka database test: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestPushSendsAuthHeaderAndBodyShape(t *testing.T) {
	type captured struct {
		method      string
		path        string
		auth        string
		contentType string
		body        []byte
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		got.body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"scan_id":"scan-123","accepted":2,"rejected":[{"path":"D:\\Bad","reason":"too long"}]}`)
	}))
	defer srv.Close()

	alloc := int64(120)
	started := time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
	payload := Payload{
		Scan: ScanJSON{
			Drive:               `D:\`,
			StartedAt:           started,
			Status:              "ok",
			Consistency:         "exact",
			VSSMode:             "required",
			TotalBytes:          123,
			TotalAllocatedBytes: &alloc,
			FolderCount:         2,
			SkippedCount:        1,
		},
		Folders: []FolderJSON{
			{Path: `D:\Data`, Depth: 1, SizeBytes: 100, AllocatedBytes: &alloc},
			{Path: `D:\Other`, Depth: 1, SizeBytes: 23, AllocatedBytes: &alloc},
		},
		Version: "1",
	}

	c := NewClient(srv.URL, "secret-key")
	ack, err := c.Push(context.Background(), payload)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if ack.ScanID != "scan-123" || ack.Accepted != 2 {
		t.Fatalf("ack tidak sesuai: %+v", ack)
	}
	if len(ack.Rejected) != 1 || ack.Rejected[0].Path != `D:\Bad` || ack.Rejected[0].Reason != "too long" {
		t.Fatalf("ack rejected tidak sesuai: %+v", ack.Rejected)
	}

	if got.method != http.MethodPost {
		t.Errorf("method = %q, mau POST", got.method)
	}
	if got.path != "/agent/v1/storage" {
		t.Errorf("path = %q, mau /agent/v1/storage", got.path)
	}
	if got.auth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, mau %q", got.auth, "Bearer secret-key")
	}
	if got.contentType != "application/json" {
		t.Errorf("Content-Type = %q, mau application/json", got.contentType)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &top); err != nil {
		t.Fatalf("body bukan JSON valid: %v (%s)", err, got.body)
	}
	assertKeys(t, "payload", top, "scan", "folders", "version")

	var scan map[string]json.RawMessage
	if err := json.Unmarshal(top["scan"], &scan); err != nil {
		t.Fatalf("scan bukan objek JSON: %v", err)
	}
	assertKeys(t, "scan", scan, "drive", "started_at", "finished_at", "status", "consistency",
		"vss_mode", "error", "total_bytes", "total_allocated_bytes", "folder_count", "skipped_count")
	if string(scan["drive"]) != `"D:\\"` {
		t.Errorf("scan.drive = %s, mau \"D:\\\\\"", scan["drive"])
	}

	var folders []map[string]json.RawMessage
	if err := json.Unmarshal(top["folders"], &folders); err != nil {
		t.Fatalf("folders bukan array JSON: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("folders = %d, mau 2", len(folders))
	}
	assertKeys(t, "folder", folders[0], "path", "depth", "size_bytes", "allocated_bytes", "file_count")
}

func assertKeys(t *testing.T, label string, obj map[string]json.RawMessage, want ...string) {
	t.Helper()
	for _, key := range want {
		if _, ok := obj[key]; !ok {
			t.Errorf("%s tidak punya field %q; ada: %v", label, key, keysOf(obj))
		}
	}
	if len(obj) != len(want) {
		t.Errorf("%s punya %d field, mau %d: %v", label, len(obj), len(want), keysOf(obj))
	}
}

func keysOf(obj map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	return keys
}

func TestPushNon2xxIncludesResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":"invalid_scan","message":"drive tidak valid"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "k")
	_, err := c.Push(context.Background(), Payload{Scan: ScanJSON{Drive: `D:\`, StartedAt: time.Now()}})
	if err == nil {
		t.Fatal("harap error pada respons non-2xx")
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "invalid_scan") {
		t.Fatalf("error tidak memuat status dan body respons: %v", err)
	}
}

func TestLoadLatestMapsPartialScan(t *testing.T) {
	store := openTestDB(t)
	db := store.DB()
	scanDate := time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)

	res, err := db.Exec(`INSERT INTO scan_history
		(scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		scanDate, `D:\`, int64(150), int64(140), "partial", 2, "vss")
	if err != nil {
		t.Fatalf("insert scan_history: %v", err)
	}
	scanID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	for _, f := range []struct {
		path  string
		depth int
		size  int64
		alloc int64
	}{
		{`D:\Data`, 1, 100, 98},
		{`D:\Other`, 1, 50, 42},
	} {
		if _, err := db.Exec(`INSERT INTO folder_snapshots
			(scan_id, scan_date, drive_letter, folder_path, depth_level, size_bytes, allocated_bytes)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			scanID, scanDate, `D:\`, f.path, f.depth, f.size, f.alloc); err != nil {
			t.Fatalf("insert folder_snapshots %s: %v", f.path, err)
		}
	}

	p, err := LoadLatest(context.Background(), db, `D:\`)
	if err != nil {
		t.Fatalf("LoadLatest error: %v", err)
	}
	if p.Scan.Drive != `D:\` {
		t.Errorf("drive = %q", p.Scan.Drive)
	}
	if p.Scan.Status != "partial" || p.Scan.Consistency != "partial" {
		t.Errorf("scan partial harus jadi status=partial consistency=partial, dapat %q/%q", p.Scan.Status, p.Scan.Consistency)
	}
	if p.Scan.TotalBytes != 150 || p.Scan.TotalAllocatedBytes == nil || *p.Scan.TotalAllocatedBytes != 140 {
		t.Errorf("total bytes salah: %d / %v", p.Scan.TotalBytes, p.Scan.TotalAllocatedBytes)
	}
	if p.Scan.FolderCount != 2 || p.Scan.SkippedCount != 2 {
		t.Errorf("folder_count/skipped_count salah: %d/%d", p.Scan.FolderCount, p.Scan.SkippedCount)
	}
	if !p.Scan.StartedAt.Equal(scanDate) {
		t.Errorf("started_at = %v, mau %v", p.Scan.StartedAt, scanDate)
	}
	if p.Scan.FinishedAt != nil {
		t.Errorf("finished_at harus nil, dapat %v", p.Scan.FinishedAt)
	}
	if p.Version == "" || p.Version != payloadVersion {
		t.Errorf("version = %q", p.Version)
	}
	if len(p.Folders) != 2 {
		t.Fatalf("folders = %d, mau 2", len(p.Folders))
	}
	if p.Folders[0].Path != `D:\Data` || p.Folders[0].SizeBytes != 100 {
		t.Errorf("folder terbesar tidak terurut/salah: %+v", p.Folders[0])
	}
	if p.Folders[0].AllocatedBytes == nil || *p.Folders[0].AllocatedBytes != 98 {
		t.Errorf("allocated_bytes folder salah: %v", p.Folders[0].AllocatedBytes)
	}
	if p.Folders[0].FileCount != nil {
		t.Errorf("file_count harus nil (tidak dicatat DiskHist), dapat %v", p.Folders[0].FileCount)
	}
}

func TestLoadLatestMapsSuccessScan(t *testing.T) {
	store := openTestDB(t)
	db := store.DB()
	scanDate := time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO scan_history
		(scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		scanDate, `E:\`, int64(10), int64(9), "success", 0, "live"); err != nil {
		t.Fatalf("insert scan_history: %v", err)
	}

	p, err := LoadLatest(context.Background(), db, `E:\`)
	if err != nil {
		t.Fatalf("LoadLatest error: %v", err)
	}
	if p.Scan.Status != "ok" || p.Scan.Consistency != "exact" {
		t.Errorf("scan sukses harus jadi status=ok consistency=exact, dapat %q/%q", p.Scan.Status, p.Scan.Consistency)
	}
	if p.Scan.VSSMode != "off" {
		t.Errorf("vss_mode = %q, mau off untuk scan live", p.Scan.VSSMode)
	}
}

func TestLoadLatestPicksNewestCompletedScan(t *testing.T) {
	store := openTestDB(t)
	db := store.DB()
	insert := func(scanDate time.Time, status string, total int64) int64 {
		t.Helper()
		res, err := db.Exec(`INSERT INTO scan_history
			(scan_date, drive_letter, total_size_bytes, total_allocated_bytes, status, skipped_count, consistency)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			scanDate, `F:\`, total, total, status, 0, "vss")
		if err != nil {
			t.Fatalf("insert scan_history: %v", err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	insert(time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), "success", 100)
	insert(time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC), "partial", 200)
	insert(time.Date(2026, 10, 7, 2, 0, 0, 0, time.UTC), "failed", 0)

	p, err := LoadLatest(context.Background(), db, `F:\`)
	if err != nil {
		t.Fatalf("LoadLatest error: %v", err)
	}
	if p.Scan.Status != "partial" || p.Scan.TotalBytes != 200 {
		t.Errorf("harus pilih scan partial terbaru (200 byte), dapat status=%q total=%d", p.Scan.Status, p.Scan.TotalBytes)
	}
}

func TestLoadLatestNoScanReturnsError(t *testing.T) {
	store := openTestDB(t)
	_, err := LoadLatest(context.Background(), store.DB(), `Z:\`)
	if err == nil {
		t.Fatal("harap error ketika belum ada scan")
	}
	if !strings.Contains(err.Error(), `Z:\`) {
		t.Errorf("error harus menyebut drive: %v", err)
	}
}
