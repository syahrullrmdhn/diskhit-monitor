package webserver

import (
	_ "embed"
	"net/http"
	"strings"
	"time"
)

//go:embed static/index.html
var dashboardHTML string

// handleDashboard menyajikan halaman HTML dashboard sesuai Definisi Selesai
// MVP (Bagian 4.4 poin 3): HTML + sedikit JS untuk fetch data dari endpoint JSON.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Serve static assets
	if strings.HasPrefix(r.URL.Path, "/static/") {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")

	// Inject current year
	html := strings.ReplaceAll(dashboardHTML, "{{YEAR}}", time.Now().Format("2006"))
	w.Write([]byte(html))
}
