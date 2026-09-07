package retention

import (
	"path/filepath"
	"testing"
	"time"

	"diskhist-monitor/internal/storage"
)

func TestAggregatePreservesMonthlyRawBaselineAndStableWeeklyData(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()

	insert := func(when time.Time, size int64) {
		res, err := db.Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count) VALUES(?, 'D:\', ?, 'success', 0)`, when, size)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		if _, err := db.Exec(`INSERT INTO folder_snapshots(scan_id,scan_date,drive_letter,folder_path,depth_level,size_bytes) VALUES(?, ?, 'D:\', 'D:\data', 1, ?)`, id, when, size); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	insert(now.AddDate(0, 0, -100), 100)
	insert(now.AddDate(0, 0, -99), 120)
	insert(now.AddDate(0, 0, -35), 200)

	r := New(db, "aggregate", 90)
	if err := r.Run(); err != nil {
		t.Fatal(err)
	}

	var raw int
	if err := db.QueryRow(`SELECT COUNT(*) FROM folder_snapshots`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != 1 {
		t.Fatalf("raw snapshots=%d, want 1 (35-day monthly baseline preserved)", raw)
	}
	var samples int
	if err := db.QueryRow(`SELECT SUM(sample_count) FROM folder_snapshots_weekly`).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if samples != 2 {
		t.Fatalf("weekly samples=%d, want 2", samples)
	}

	if err := r.Run(); err != nil {
		t.Fatal(err)
	}
	var samplesAgain int
	if err := db.QueryRow(`SELECT SUM(sample_count) FROM folder_snapshots_weekly`).Scan(&samplesAgain); err != nil {
		t.Fatal(err)
	}
	if samplesAgain != samples {
		t.Fatalf("weekly aggregate changed on rerun: %d -> %d", samples, samplesAgain)
	}
}

func TestAggregateMergesNewlyEligibleSamplesIntoExistingWeek(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "merge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	now := time.Now().UTC()
	weekStart := now.AddDate(0, 0, -100)
	week := weekStart.AddDate(0, 0, -int(weekStart.Weekday())+1).Format("2006-01-02")
	if _, err := db.Exec(`INSERT INTO folder_snapshots_weekly VALUES('D:\data','D:\',?,100,100,100,1)`, week); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count) VALUES(?, 'D:\', 200, 'success', 0)`, weekStart.AddDate(0, 0, 1))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO folder_snapshots(scan_id,scan_date,drive_letter,folder_path,depth_level,size_bytes) VALUES(?,?,'D:\','D:\data',1,200)`, id, weekStart.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if err := New(db, "aggregate", 90).Run(); err != nil {
		t.Fatal(err)
	}
	var samples int
	if err := db.QueryRow(`SELECT sample_count FROM folder_snapshots_weekly WHERE folder_path='D:\data'`).Scan(&samples); err != nil {
		t.Fatal(err)
	}
	if samples != 2 {
		t.Fatalf("samples=%d, want 2", samples)
	}
}
