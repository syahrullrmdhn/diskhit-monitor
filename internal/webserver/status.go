package webserver

import (
	"sync"
	"time"
)

// StatusTracker menyimpan status terkini proses, dibaca oleh /healthz (NFR-07).
// Aman dipakai concurrent karena dilindungi mutex - scanner goroutine menulis,
// HTTP handler goroutine membaca, bisa terjadi bersamaan.
type StatusTracker struct {
	mu            sync.RWMutex
	startTime     time.Time
	lastScanTime  time.Time
	lastScanStatus string
	skippedCount  int
}

func NewStatusTracker() *StatusTracker {
	return &StatusTracker{startTime: time.Now(), lastScanStatus: "belum pernah scan"}
}

func (st *StatusTracker) RecordScan(t time.Time, status string, skipped int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.lastScanTime = t
	st.lastScanStatus = status
	st.skippedCount = skipped
}

func (st *StatusTracker) Snapshot() (uptime time.Duration, lastScan time.Time, status string, skipped int) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return time.Since(st.startTime), st.lastScanTime, st.lastScanStatus, st.skippedCount
}
