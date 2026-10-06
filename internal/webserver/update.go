package webserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

type updateProcess interface {
	Wait() error
}

var (
	updateGOOS    = runtime.GOOS
	launchUpdater = startUpdater
)

type updateState struct {
	mu         sync.Mutex
	running    bool
	startedAt  time.Time
	finishedAt time.Time
	exitCode   *int
	errText    string
}

type updateStatusResponse struct {
	Running    bool       `json:"running"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func startUpdater() (updateProcess, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate application: %w", err)
	}
	path := filepath.Join(filepath.Dir(exe), "update-diskhist.bat")
	cmd := exec.Command("cmd.exe", "/c", path)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSONError(w, "method not allowed", 405)
		return
	}
	if updateGOOS != "windows" {
		writeJSONError(w, "self-update is only supported on Windows", 501)
		return
	}

	s.update.mu.Lock()
	if s.update.running {
		s.update.mu.Unlock()
		writeJSONError(w, "an update is already running", 409)
		return
	}
	s.update.running = true
	s.update.startedAt = time.Now().UTC()
	s.update.finishedAt = time.Time{}
	s.update.exitCode = nil
	s.update.errText = ""
	s.update.mu.Unlock()

	process, err := launchUpdater()
	if err != nil {
		s.update.mu.Lock()
		s.update.running = false
		s.update.finishedAt = time.Now().UTC()
		s.update.errText = "failed to start updater"
		s.update.mu.Unlock()
		writeJSONError(w, "failed to start updater", 500)
		return
	}
	go s.finishUpdate(process)

	writeJSON(w, 202, map[string]any{"accepted": true, "status": "running"})
}

func (s *Server) finishUpdate(process updateProcess) {
	err := process.Wait()
	code := 0
	if err != nil {
		code = 1
	}
	s.update.mu.Lock()
	s.update.running = false
	s.update.finishedAt = time.Now().UTC()
	s.update.exitCode = &code
	if err != nil {
		s.update.errText = "updater failed"
	}
	s.update.mu.Unlock()
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, _ *http.Request) {
	s.update.mu.Lock()
	defer s.update.mu.Unlock()
	var started, finished *time.Time
	if !s.update.startedAt.IsZero() {
		t := s.update.startedAt
		started = &t
	}
	if !s.update.finishedAt.IsZero() {
		t := s.update.finishedAt
		finished = &t
	}
	writeJSON(w, 200, updateStatusResponse{Running: s.update.running, StartedAt: started, FinishedAt: finished, ExitCode: s.update.exitCode})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, message string, status int) {
	writeJSON(w, status, map[string]string{"error": message})
}
