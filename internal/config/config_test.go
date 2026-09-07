package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadAppliesSafeAccuracyDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 90}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: test.db
log_path: test.log
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Accuracy.VSSMode != "required" {
		t.Fatalf("vss_mode=%q", cfg.Accuracy.VSSMode)
	}
	if !cfg.Accuracy.CollectAllocatedSize {
		t.Fatal("allocated size should default enabled")
	}
	if cfg.Resources.ScanTimeout != 2*time.Hour {
		t.Fatalf("scan timeout=%s", cfg.Resources.ScanTimeout)
	}
	if cfg.Resources.CommandTimeout != 2*time.Minute {
		t.Fatalf("command timeout=%s", cfg.Resources.CommandTimeout)
	}
	if !cfg.Resources.SingleScan {
		t.Fatal("single scan guard should default enabled")
	}
}

func TestLoadRejectsUnsafeAccuracyConfig(t *testing.T) {
	base := `drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 90}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: test.db
log_path: test.log
accuracy: {vss_mode: %s}
`
	for _, mode := range []string{"best-effort", "off; calc"} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(strings.Replace(base, "%s", mode, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("mode %q accepted", mode)
		}
	}
}
