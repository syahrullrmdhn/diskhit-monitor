package scanner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"diskhist-monitor/internal/storage"
)

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectFolderSizesIncludesEveryAncestor(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "parent", "a.bin"), 10)
	writeSizedFile(t, filepath.Join(root, "parent", "child", "b.bin"), 25)
	writeSizedFile(t, filepath.Join(root, "parent", "child", "deep", "c.bin"), 7)

	sizes, total, skipped := collectFolderSizes(root, 3)
	if skipped != 0 {
		t.Fatalf("skipped=%d, want 0", skipped)
	}
	if total != 42 {
		t.Fatalf("total=%d, want 42", total)
	}
	want := map[string]int64{
		filepath.Clean(root):                           42,
		filepath.Join(root, "parent"):                  42,
		filepath.Join(root, "parent", "child"):         32,
		filepath.Join(root, "parent", "child", "deep"): 7,
	}
	for path, expected := range want {
		if got := sizes[path]; got != expected {
			t.Errorf("size[%q]=%d, want %d", path, got, expected)
		}
	}
}

func TestCollectFolderSizesRollsFilesBelowMaxDepthIntoVisibleAncestors(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "one", "two", "three", "four", "data.bin"), 99)

	sizes, total, _ := collectFolderSizes(root, 2)
	if total != 99 {
		t.Fatalf("total=%d, want 99", total)
	}
	if got := sizes[filepath.Join(root, "one", "two")]; got != 99 {
		t.Fatalf("max-depth folder size=%d, want 99", got)
	}
	if _, exists := sizes[filepath.Join(root, "one", "two", "three")]; exists {
		t.Fatal("folder deeper than max_depth must not be emitted")
	}
}

func TestCollectFolderMetricsMapsSourcePathsToReportedRoot(t *testing.T) {
	source := t.TempDir()
	reported := filepath.Join(t.TempDir(), "reported")
	writeSizedFile(t, filepath.Join(source, "one", "data.bin"), 11)

	metrics, total, skipped := collectFolderMetrics(source, reported, 2, false)
	if skipped != 0 || total.Logical != 11 || total.Allocated != 11 {
		t.Fatalf("total=%+v skipped=%d", total, skipped)
	}
	if got := metrics[filepath.Join(reported, "one")]; got.Logical != 11 {
		t.Fatalf("reported metric=%+v", got)
	}
	if _, exists := metrics[filepath.Join(source, "one")]; source != reported && exists {
		t.Fatal("source snapshot path leaked into reported metrics")
	}
}

func TestPersistKeepsUnchangedFolderActiveAndStoresEverySnapshot(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store.DB(), 3)
	root := filepath.Clean(t.TempDir())
	sizes := map[string]int64{root: 42}
	first := Result{Drive: root, TotalSizeBytes: 42, Status: "success", ScanDate: time.Now().Add(-time.Hour)}
	second := Result{Drive: root, TotalSizeBytes: 42, Status: "success", ScanDate: time.Now()}
	if err := s.persist(root, sizes, first); err != nil {
		t.Fatal(err)
	}
	if err := s.persist(root, sizes, second); err != nil {
		t.Fatal(err)
	}
	var active, snapshots int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM folder_metrics WHERE folder_path=? AND is_active=1`, root).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM folder_snapshots WHERE folder_path=?`, root).Scan(&snapshots); err != nil {
		t.Fatal(err)
	}
	if active != 1 || snapshots != 2 {
		t.Fatalf("active=%d snapshots=%d, want 1 and 2", active, snapshots)
	}
}

func TestPersistVersionsAllocatedSizeChange(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "allocated.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := New(store.DB(), 2)
	root := filepath.Clean(t.TempDir())
	first := Result{Drive: root, TotalSizeBytes: 42, TotalAllocatedBytes: 64, Status: "success", Consistency: "live", ScanDate: time.Now().Add(-time.Hour)}
	second := Result{Drive: root, TotalSizeBytes: 42, TotalAllocatedBytes: 128, Status: "success", Consistency: "live", ScanDate: time.Now()}
	if err := s.persistMetrics(root, map[string]folderMetric{root: {Logical: 42, Allocated: 64}}, first); err != nil {
		t.Fatal(err)
	}
	if err := s.persistMetrics(root, map[string]folderMetric{root: {Logical: 42, Allocated: 128}}, second); err != nil {
		t.Fatal(err)
	}
	var active, oldInactive int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM folder_metrics WHERE folder_path=? AND is_active=1 AND allocated_bytes=128`, root).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM folder_metrics WHERE folder_path=? AND is_active=0 AND allocated_bytes=64`, root).Scan(&oldInactive); err != nil {
		t.Fatal(err)
	}
	if active != 1 || oldInactive != 1 {
		t.Fatalf("active=%d oldInactive=%d, want 1/1", active, oldInactive)
	}
}

func TestFailedVSSCreationIsPersisted(t *testing.T) {
	store, err := storage.Open(filepath.Join(t.TempDir(), "failed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := NewWithOptions(store.DB(), 2, Options{UseVSS: true, ScanTimeout: time.Minute, CommandTimeout: time.Second})
	s.provider = failingSnapshotProvider{}
	result := s.ScanDriveContext(context.Background(), `D:\`)
	if result.Status != "failed" {
		t.Fatalf("status=%q", result.Status)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT COUNT(*) FROM scan_history WHERE status='failed' AND consistency='failed'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("failed history count=%d, want 1", count)
	}
}

type failingSnapshotProvider struct{}

func (failingSnapshotProvider) Create(context.Context, string) (Snapshot, error) {
	return Snapshot{}, errors.New("vss unavailable")
}
func (failingSnapshotProvider) Delete(context.Context, Snapshot) error { return nil }
