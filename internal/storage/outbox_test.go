package storage

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestOpenMigratesDurableScanOutbox(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var name string
	if err := store.DB().QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='scan_outbox'`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "scan_outbox" {
		t.Fatalf("table=%q", name)
	}
}

func TestEnqueueScanOutboxIsIdempotent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := store.DB().Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count,consistency,finished_at) VALUES(?,?,?,?,?,?,?)`, time.Now(), `D:\`, 1, "success", 0, "live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	key := "diskhist:agent:D:\\:1"
	if err := EnqueueScanOutbox(store.DB(), "agent", `D:\`, id); err != nil {
		t.Fatal(err)
	}
	if err := EnqueueScanOutbox(store.DB(), "agent", `D:\`, id); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM scan_outbox WHERE idempotency_key=?`, key).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("count=%d", count)
	}
}

func TestClaimAndMarkOutbox(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := store.DB().Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count,consistency,finished_at) VALUES(?,?,?,?,?,?,?)`, time.Now(), `E:\`, 1, "success", 0, "live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if err := EnqueueScanOutbox(store.DB(), "a", `E:\`, id); err != nil {
		t.Fatal(err)
	}
	item, err := ClaimOutbox(store.DB(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if item.IdempotencyKey != "diskhist:a:E:\\:"+strconv.FormatInt(id, 10) {
		t.Fatalf("key=%q", item.IdempotencyKey)
	}
	if err := MarkOutboxSent(store.DB(), item.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM scan_outbox WHERE id=?`, item.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "sent" {
		t.Fatalf("status=%q", status)
	}
}

func TestOutboxScanIDForeignKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.DB().Exec(`INSERT INTO scan_outbox(agent_id,drive_letter,scan_id,idempotency_key,status,attempts,created_at) VALUES('a','D:\\',999,'x','pending',0,?)`, time.Now())
	if err == nil {
		t.Fatal("expected foreign key error")
	}
}

func TestOutboxSupportsDeadLetterAndDoesNotReclaimIt(t *testing.T) {
	store := openTestStore(t)
	id := insertTestScan(t, store.DB(), `F:\\`)
	if err := EnqueueScanOutbox(store.DB(), "a", `F:\\`, id); err != nil {
		t.Fatal(err)
	}
	item, err := ClaimOutbox(store.DB(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := MarkOutboxDeadLetter(store.DB(), item.ID, "permanent failure", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimOutbox(store.DB(), time.Now().UTC()); err == nil {
		t.Fatal("dead-letter item was claimable")
	}
	var status string
	if err := store.DB().QueryRow(`SELECT status FROM scan_outbox WHERE id=?`, item.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "dead_letter" {
		t.Fatalf("status=%q", status)
	}
}

func TestClaimOutboxAllowsOnlyOneConcurrentClaimAndReclaimsExpiredLease(t *testing.T) {
	store := openTestStore(t)
	id := insertTestScan(t, store.DB(), `G:\\`)
	if err := EnqueueScanOutbox(store.DB(), "a", `G:\\`, id); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := ClaimOutbox(store.DB(), now); errs <- err }()
	}
	wg.Wait()
	close(errs)
	claimed := 0
	for err := range errs {
		if err == nil {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent claims=%d", claimed)
	}
	if _, err := store.DB().Exec(`UPDATE scan_outbox SET lease_until=? WHERE id=?`, now.Add(-time.Minute), id); err != nil {
		t.Fatal(err)
	}
	if _, err := ClaimOutbox(store.DB(), now.Add(time.Minute)); err != nil {
		t.Fatalf("expired lease was not reclaimable: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "diskhist.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func insertTestScan(t *testing.T, db *sql.DB, drive string) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count,consistency,finished_at) VALUES(?,?,?,?,?,?,?)`, time.Now(), drive, 1, "success", 0, "live", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}
