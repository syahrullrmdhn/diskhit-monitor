package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesNdelikAndResolvesRelativePaths(t *testing.T) {
	path := writeConfig(t, `config_version: 1
drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 90}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: data/diskhist.db
log_path: logs/agent.log
ndelik:
  server: https://ndelik.example.test
  agent_id: agent-123
  api_key_ref: env://DISKHIST_API_KEY
  enrollment_token_ref: file://secrets/enrollment.token
  data_dir: state
  enabled: true
`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	dir := filepath.Dir(path)
	if cfg.DBPath != filepath.Join(dir, "data", "diskhist.db") || cfg.LogPath != filepath.Join(dir, "logs", "agent.log") {
		t.Fatalf("paths not resolved relative to config: db=%q log=%q", cfg.DBPath, cfg.LogPath)
	}
	if cfg.Ndelik.DataDir != filepath.Join(dir, "state") || cfg.Ndelik.APIKeyRef.Scheme != "env" || cfg.Ndelik.APIKeyRef.Value != "DISKHIST_API_KEY" {
		t.Fatalf("unexpected ndelik config: %+v", cfg.Ndelik)
	}
	if cfg.Ndelik.EnrollmentTokenRef.String() != "file://secrets/enrollment.token" { t.Fatal("secret reference was resolved unexpectedly") }
}

func TestLoadPreservesExplicitFalseAndRejectsUnknownFields(t *testing.T) {
	path := writeConfig(t, `config_version: 1
drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 90}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: test.db
log_path: test.log
resources: {single_scan: false}
`)
	cfg, err := Load(path)
	if err != nil { t.Fatal(err) }
	if cfg.Resources.SingleScan { t.Fatal("explicit single_scan=false was overwritten") }
	bad := writeConfig(t, "drives: [\"D:\\\\\"]\nmax_depth: 1\nunknown: true\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "unknown") { t.Fatalf("unknown field was accepted: %v", err) }
}

func TestLoadRejectsMalformedSecretReference(t *testing.T) {
	path := writeConfig(t, `config_version: 1
drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 90}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: test.db
log_path: test.log
ndelik: {enabled: true, server: https://example.test, agent_id: a, api_key_ref: plaintext-secret}
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "secret reference") { t.Fatalf("malformed secret ref accepted: %v", err) }
}

func TestResolveConfigPathUsesExecutableDirectory(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "bin", "diskhist.exe")
	got := ResolveConfigPath("config.yaml", exe)
	want := filepath.Join(filepath.Dir(exe), "config.yaml")
	if got != want { t.Fatalf("got %q want %q", got, want) }
}

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

func TestLoadMigratesLegacyThirtyDayRetention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := `drives: ["D:\\"]
max_depth: 5
schedule: "0 2 * * *"
retention: {mode: aggregate, retain_raw_days: 30}
web_server: {port: 8787, bind_address: 127.0.0.1}
database_path: test.db
log_path: test.log
`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("legacy v1.0 config must remain valid after update: %v", err)
	}
	if cfg.Retention.RetainRawDays != 31 {
		t.Fatalf("legacy retention should migrate to 31, got %d", cfg.Retention.RetainRawDays)
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
