package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"diskhist-monitor/internal/config"
	"diskhist-monitor/internal/report"
	"diskhist-monitor/internal/storage"
)

func TestResolveSecretReadsEnvironmentAndFile(t *testing.T) {
	t.Setenv("DISKHIST_TEST_SECRET", "from-env")
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveSecret(config.SecretRef{Scheme: "env", Value: "DISKHIST_TEST_SECRET"}); err != nil || got != "from-env" {
		t.Fatalf("env secret = %q, %v", got, err)
	}
	if got, err := resolveSecret(config.SecretRef{Scheme: "file", Value: filepath.Base(path)}, filepath.Dir(path)); err != nil || got != "from-file" {
		t.Fatalf("relative file secret = %q, %v", got, err)
	}
}

func TestReportWorkerPushesClaimedScanByExactID(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	oldScan, newScan := insertWorkerScan(t, store.DB(), `D:\`, 1), insertWorkerScan(t, store.DB(), `D:\`, 2)
	if err := storage.EnqueueScanOutbox(store.DB(), "agent", `D:\`, oldScan); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnqueueScanOutbox(store.DB(), "agent", `D:\`, newScan); err != nil {
		t.Fatal(err)
	}
	var gotKey atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload report.Payload
		if err := decodeJSON(r, &payload); err != nil {
			t.Errorf("decode: %v", err)
		}
		gotKey.Store(payload.IdempotencyKey)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scan_id":"remote-1","accepted":1}`))
	}))
	defer ts.Close()
	client := report.NewClient(ts.URL, "key")
	client.Retry.MaxAttempts = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := newReportWorker(store.DB(), client, 5*time.Millisecond, 3)
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	waitForWorker(t, store.DB(), 2*time.Second)
	cancel()
	<-done
	var sent int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM scan_outbox WHERE status='sent'`).Scan(&sent); err != nil {
		t.Fatal(err)
	}
	if sent != 2 {
		t.Fatalf("sent=%d", sent)
	}
	if gotKey.Load() == nil {
		t.Fatal("worker did not push a payload")
	}
}

func TestReportWorkerMakesConfiguredFinalAttempt(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scanID := insertWorkerScan(t, store.DB(), `D:\`, 1)
	if err := storage.EnqueueScanOutbox(store.DB(), "agent", `D:\`, scanID); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scan_id":"remote-1","accepted":1}`))
	}))
	defer ts.Close()
	client := report.NewClient(ts.URL, "key")
	client.Retry.MaxAttempts = 1
	worker := newReportWorker(store.DB(), client, time.Millisecond, 3)
	ctx := context.Background()

	if err := worker.processOne(ctx); err == nil {
		t.Fatal("first attempt unexpectedly succeeded")
	}
	if err := worker.processOne(ctx); err == nil {
		t.Fatal("second attempt unexpectedly succeeded")
	}
	if err := worker.processOne(ctx); err != nil {
		t.Fatalf("final attempt failed: %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("push calls=%d, want 3", got)
	}
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM scan_outbox WHERE scan_id=?`, scanID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("outbox status=%q, want sent", status)
	}
}

func TestReportWorkerReturnsOnCancellation(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	worker := newReportWorker(store.DB(), report.NewClient("http://127.0.0.1:1", "key"), time.Hour, 3)
	done := make(chan struct{})
	go func() { worker.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func decodeJSON(r *http.Request, dst any) error { return json.NewDecoder(r.Body).Decode(dst) }

func insertWorkerScan(t *testing.T, db *sql.DB, drive string, size int64) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,total_allocated_bytes,status,skipped_count,consistency,finished_at) VALUES(?,?,?,?,?,?,?,?)`, time.Now().UTC(), drive, size, size, "success", 0, "live", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO folder_snapshots(scan_id,scan_date,drive_letter,folder_path,depth_level,size_bytes,allocated_bytes) VALUES(?,?,?,?,?,?,?)`, id, time.Now().UTC(), drive, drive, 0, size, size); err != nil {
		t.Fatal(err)
	}
	return id
}

func waitForWorker(t *testing.T, db *sql.DB, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var pending int
		if err := db.QueryRow(`SELECT COUNT(*) FROM scan_outbox WHERE status!='sent'`).Scan(&pending); err == nil && pending == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("outbox was not drained")
}
