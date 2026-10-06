package webserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"diskhist-monitor/internal/storage"
)

type fakeUpdateProcess struct{}

func (fakeUpdateProcess) Wait() error { return nil }

type blockingUpdateProcess struct{ done <-chan struct{} }

func (p blockingUpdateProcess) Wait() error { <-p.done; return errors.New("stopped") }

func TestUpdateRejectsNonWindows(t *testing.T) {
	srv := &Server{}
	oldGOOS := updateGOOS
	updateGOOS = "linux"
	t.Cleanup(func() { updateGOOS = oldGOOS })
	rec := httptest.NewRecorder()
	srv.handleUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/update", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpdateAcceptsAndTracksDetachedUpdater(t *testing.T) {
	srv := &Server{}
	oldGOOS, oldLaunch := updateGOOS, launchUpdater
	updateGOOS = "windows"
	started := make(chan struct{})
	launchUpdater = func() (updateProcess, error) { close(started); return fakeUpdateProcess{}, nil }
	t.Cleanup(func() { updateGOOS, launchUpdater = oldGOOS, oldLaunch })
	rec := httptest.NewRecorder()
	srv.handleUpdate(rec, httptest.NewRequest(http.MethodPost, "/api/update", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	<-started
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["accepted"] != true || body["status"] != "running" {
		t.Fatalf("unexpected response: %s", rec.Body.String())
	}
}

func TestUpdateRejectsConcurrentRun(t *testing.T) {
	srv := &Server{}
	oldGOOS, oldLaunch := updateGOOS, launchUpdater
	updateGOOS = "windows"
	block := make(chan struct{})
	launchUpdater = func() (updateProcess, error) { return blockingUpdateProcess{done: block}, nil }
	t.Cleanup(func() { close(block); updateGOOS, launchUpdater = oldGOOS, oldLaunch })
	first := httptest.NewRecorder()
	srv.handleUpdate(first, httptest.NewRequest(http.MethodPost, "/api/update", nil))
	second := httptest.NewRecorder()
	srv.handleUpdate(second, httptest.NewRequest(http.MethodPost, "/api/update", nil))
	if second.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", second.Code, second.Body.String())
	}
}

func TestUpdateStatusDoesNotExposeUpdaterDetails(t *testing.T) {
	srv := &Server{}
	srv.update.running = true
	srv.update.startedAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	srv.update.errText = "secret-token-and-path"
	rec := httptest.NewRecorder()
	srv.handleUpdateStatus(rec, httptest.NewRequest(http.MethodGet, "/api/update-status", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "secret-token-and-path") {
		t.Fatalf("unsafe status response: %d %s", rec.Code, rec.Body.String())
	}
}

func TestParsePeriod(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"day", 1}, {"week", 7}, {"month", 30}, {"", 7}, {"invalid", 7},
	}
	for _, tc := range tests {
		if got := parsePeriodDays(tc.input); got != tc.want {
			t.Errorf("parsePeriodDays(%q)=%d, want %d", tc.input, got, tc.want)
		}
	}
}

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store.DB()
}

func addSnapshot(t *testing.T, db *sql.DB, when time.Time, path string, size int64) {
	t.Helper()
	res, err := db.Exec(`INSERT INTO scan_history(scan_date,drive_letter,total_size_bytes,status,skipped_count)
		VALUES(?, 'D:\', ?, 'success', 0)`, when, size)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO folder_snapshots(scan_id,scan_date,drive_letter,folder_path,depth_level,size_bytes)
		VALUES(?, ?, 'D:\', ?, 1, ?)`, id, when, path, size); err != nil {
		t.Fatal(err)
	}
}

func TestTopGrowersComparesLatestToPeriodBaseline(t *testing.T) {
	db := openTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	addSnapshot(t, db, now.AddDate(0, 0, -8), `D:\data`, 100)
	addSnapshot(t, db, now, `D:\data`, 125)

	srv := &Server{db: db}
	req := httptest.NewRequest("GET", "/api/top-growers?days=7", nil)
	rec := httptest.NewRecorder()
	srv.handleTopGrowers(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got []struct {
		Path        string  `json:"path"`
		GrowthBytes int64   `json:"growth_bytes"`
		GrowthPct   float64 `json:"growth_pct"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Path != `D:\data` || got[0].GrowthBytes != 25 || got[0].GrowthPct != 25 {
		t.Fatalf("unexpected response: %+v", got)
	}
}

func TestTopGrowersUsesLatestScanTimeNotWallClock(t *testing.T) {
	db := openTestDB(t)
	latest := time.Now().UTC().AddDate(0, 0, -20).Truncate(time.Second)
	addSnapshot(t, db, latest.AddDate(0, 0, -8), `D:\offline`, 100)
	addSnapshot(t, db, latest, `D:\offline`, 150)
	srv := &Server{db: db}
	req := httptest.NewRequest("GET", "/api/top-growers?days=7", nil)
	rec := httptest.NewRecorder()
	srv.handleTopGrowers(rec, req)
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected comparison despite service downtime, got %s", rec.Body.String())
	}
}

func TestFolderHistoryKeepsLegacyFields(t *testing.T) {
	db := openTestDB(t)
	addSnapshot(t, db, time.Now().UTC(), `D:\compat`, 10)
	srv := &Server{db: db}
	req := httptest.NewRequest("GET", "/api/folder-history?path="+url.QueryEscape(`D:\compat`), nil)
	rec := httptest.NewRecorder()
	srv.handleFolderHistory(rec, req)
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0]["scan_date"] == nil || got[0]["start_date"] == nil || got[0]["is_active"] != true {
		t.Fatalf("compatibility fields missing: %s", rec.Body.String())
	}
}

func TestTopGrowersDoesNotInventGrowthWithoutBaseline(t *testing.T) {
	db := openTestDB(t)
	addSnapshot(t, db, time.Now().UTC(), `D:\new`, 500)
	srv := &Server{db: db}
	req := httptest.NewRequest("GET", "/api/top-growers?days=30", nil)
	rec := httptest.NewRecorder()
	srv.handleTopGrowers(rec, req)
	var got []any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no comparison without baseline, got %s", rec.Body.String())
	}
}
