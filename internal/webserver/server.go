package webserver

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Server membungkus http.Server dan status kesehatan internal
// yang dipakai endpoint /healthz (NFR-07).
type Server struct {
	httpServer *http.Server
	status     *StatusTracker
	db         *sql.DB
}

// New membuat web server yang BIND KE LOCALHOST SAJA (127.0.0.1),
// sesuai keputusan final: tidak bisa diakses dari device lain di LAN.
func New(bindAddress string, port int, status *StatusTracker, db *sql.DB) *Server {
	mux := http.NewServeMux()

	srv := &Server{
		httpServer: &http.Server{
			Addr:         fmt.Sprintf("%s:%d", bindAddress, port),
			Handler:      mux,
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 30 * time.Second,
		},
		status: status,
		db:     db,
	}

	mux.HandleFunc("/healthz", srv.recoverMiddleware(srv.handleHealthz))
	mux.HandleFunc("/api/top-growers", srv.recoverMiddleware(srv.handleTopGrowers))
	mux.HandleFunc("/api/folder-history", srv.recoverMiddleware(srv.handleFolderHistory))
	mux.HandleFunc("/api/export-csv", srv.recoverMiddleware(srv.handleExportCSV))
	mux.HandleFunc("/api/scan-history", srv.recoverMiddleware(srv.handleScanHistory))
	mux.HandleFunc("/", srv.recoverMiddleware(srv.handleDashboard))

	return srv
}

// recoverMiddleware membungkus setiap handler dengan recover(), sesuai NFR-06:
// panic pada satu request TIDAK boleh menjatuhkan seluruh web server.
func (s *Server) recoverMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[ERROR] panic ditangkap pada handler %q: %v (request tetap direspon error, server tidak crash)", r.URL.Path, rec)
				http.Error(w, "internal error - lihat log server untuk detail", http.StatusInternalServerError)
			}
		}()
		next(w, r)
	}
}

// Start menjalankan web server. Dipanggil dalam goroutine terpisah dari
// scanner (FR-09: web server harus tetap responsif independen dari scan).
func (s *Server) Start() error {
	log.Printf("[INFO] web server berjalan di %s (hanya bisa diakses dari komputer ini)", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("web server error di %s: %w (kemungkinan port sudah dipakai proses lain)", s.httpServer.Addr, err)
	}
	return nil
}

// Shutdown melakukan graceful shutdown web server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
