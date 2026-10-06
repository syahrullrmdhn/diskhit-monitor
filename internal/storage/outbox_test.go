package storage

import (
	"path/filepath"
	"strconv"
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
