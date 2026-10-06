package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config merepresentasikan seluruh isi config.yaml.
// Field ini harus selaras dengan PRD/SRS Bagian 2.5.
type Config struct {
	ConfigVersion int       `yaml:"config_version"`
	Drives        []string  `yaml:"drives"`
	MaxDepth      int       `yaml:"max_depth"`
	Schedule      string    `yaml:"schedule"`
	Retention     Retention `yaml:"retention"`
	WebServer     WebServer `yaml:"web_server"`
	Accuracy      Accuracy  `yaml:"accuracy"`
	Resources     Resources `yaml:"resources"`
	DBPath        string    `yaml:"database_path"`
	LogPath       string    `yaml:"log_path"`
	Ndelik        Ndelik    `yaml:"ndelik"`
}

type SecretRef struct {
	Scheme string
	Value  string
}

func (r *SecretRef) UnmarshalYAML(node *yaml.Node) error {
	var value string
	if err := node.Decode(&value); err != nil {
		return fmt.Errorf("secret reference must be a string: %w", err)
	}
	parts := strings.SplitN(value, "://", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`).MatchString(parts[0]) {
		return fmt.Errorf("invalid secret reference %q; use scheme://name", value)
	}
	if parts[0] != "env" && parts[0] != "file" {
		return fmt.Errorf("unsupported secret reference scheme %q", parts[0])
	}
	r.Scheme, r.Value = parts[0], parts[1]
	return nil
}
func (r SecretRef) String() string {
	if r.Scheme == "" {
		return ""
	}
	return r.Scheme + "://" + r.Value
}

type Ndelik struct {
	Server             string    `yaml:"server"`
	AgentID            string    `yaml:"agent_id"`
	APIKeyRef          SecretRef `yaml:"api_key_ref"`
	EnrollmentTokenRef SecretRef `yaml:"enrollment_token_ref"`
	DataDir            string    `yaml:"data_dir"`
	Enabled            bool      `yaml:"enabled"`
}

type Accuracy struct {
	VSSMode              string `yaml:"vss_mode"`
	CollectAllocatedSize bool   `yaml:"collect_allocated_size"`
	allocatedConfigured  bool
}

func (a *Accuracy) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		VSSMode              string `yaml:"vss_mode"`
		CollectAllocatedSize *bool  `yaml:"collect_allocated_size"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	a.VSSMode = raw.VSSMode
	if raw.CollectAllocatedSize != nil {
		a.CollectAllocatedSize = *raw.CollectAllocatedSize
		a.allocatedConfigured = true
	}
	return nil
}

type Resources struct {
	ScanTimeout          time.Duration `yaml:"-"`
	CommandTimeout       time.Duration `yaml:"-"`
	SingleScan           bool          `yaml:"single_scan"`
	singleScanConfigured bool
}

func (r *Resources) UnmarshalYAML(node *yaml.Node) error {
	var raw struct {
		ScanTimeout    string `yaml:"scan_timeout"`
		CommandTimeout string `yaml:"command_timeout"`
		SingleScan     *bool  `yaml:"single_scan"`
	}
	if err := node.Decode(&raw); err != nil {
		return err
	}
	var err error
	if raw.ScanTimeout != "" {
		r.ScanTimeout, err = time.ParseDuration(raw.ScanTimeout)
		if err != nil {
			return fmt.Errorf("field 'resources.scan_timeout': %w", err)
		}
	}
	if raw.CommandTimeout != "" {
		r.CommandTimeout, err = time.ParseDuration(raw.CommandTimeout)
		if err != nil {
			return fmt.Errorf("field 'resources.command_timeout': %w", err)
		}
	}
	if raw.SingleScan != nil {
		r.SingleScan = *raw.SingleScan
		r.singleScanConfigured = true
	}
	return nil
}

type Retention struct {
	Mode          string `yaml:"mode"` // "aggregate" atau "delete"
	RetainRawDays int    `yaml:"retain_raw_days"`
}

type WebServer struct {
	Port        int    `yaml:"port"`
	BindAddress string `yaml:"bind_address"`
}

// Load membaca dan memvalidasi config.yaml sesuai NFR-05.
// Jika ada masalah, error yang dikembalikan HARUS spesifik (bukan generik),
// sesuai NFR-08, agar caller bisa langsung log FATAL dengan detail jelas.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gagal membaca file config di %q: %w (pastikan file ada dan bisa dibaca)", path, err)
	}

	var cfg Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config.yaml di %q tidak valid (syntax YAML error): %w", path, err)
	}

	cfg.applyDefaults()
	baseDir := filepath.Dir(path)
	cfg.DBPath = resolvePath(baseDir, cfg.DBPath)
	cfg.LogPath = resolvePath(baseDir, cfg.LogPath)
	cfg.Ndelik.DataDir = resolvePath(baseDir, cfg.Ndelik.DataDir)
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config.yaml di %q lolos parsing tapi nilainya tidak valid: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults mengisi nilai safe default untuk field accuracy/resources
// yang tidak ada di file config, agar perilaku aman (VSS required, resource
// bounds) berlaku tanpa perlu user mengubah config lama.
func (c *Config) applyDefaults() {
	if c.ConfigVersion == 0 {
		c.ConfigVersion = 1
	}
	if c.Retention.RetainRawDays == 30 {
		// v1.0.x shipped with 30 days. Preserve upgrade compatibility while
		// retaining enough history for a true monthly baseline.
		c.Retention.RetainRawDays = 31
	}
	if c.Accuracy.VSSMode == "" {
		c.Accuracy.VSSMode = "required"
	}
	if !c.Accuracy.allocatedConfigured {
		c.Accuracy.CollectAllocatedSize = true
	}
	if c.Resources.ScanTimeout == 0 {
		c.Resources.ScanTimeout = 2 * time.Hour
	}
	if c.Resources.CommandTimeout == 0 {
		c.Resources.CommandTimeout = 2 * time.Minute
	}
	if !c.Resources.singleScanConfigured {
		c.Resources.SingleScan = true
	}
}

func resolvePath(base, value string) string {
	if value == "" || filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(base, value)
}

func ResolveConfigPath(name, executable string) string {
	if filepath.IsAbs(name) {
		return name
	}
	return filepath.Join(filepath.Dir(executable), name)
}

var driveRootPattern = regexp.MustCompile(`^[A-Za-z]:\\$`)

func validateDriveRoot(drive string) error {
	if !driveRootPattern.MatchString(drive) {
		return fmt.Errorf("drive %q tidak valid; harus root volume lokal seperti D:\\\\ (UNC dan subfolder tidak didukung oleh mode VSS)", drive)
	}
	return nil
}

// validate memastikan nilai-nilai config masuk akal.
// Sesuai NFR-05: TIDAK boleh diam-diam pakai default jika nilai mencurigakan -
// harus return error spesifik supaya user tahu persis apa yang perlu diperbaiki.
func (c *Config) validate() error {
	if len(c.Drives) == 0 {
		return fmt.Errorf("field 'drives' kosong - minimal harus ada 1 drive yang dipantau, contoh: drives: [\"D:\\\\\"]")
	}
	for _, drive := range c.Drives {
		if err := validateDriveRoot(drive); err != nil {
			return err
		}
	}
	if c.MaxDepth <= 0 {
		return fmt.Errorf("field 'max_depth' bernilai %d, harus lebih besar dari 0", c.MaxDepth)
	}
	if c.Retention.Mode != "aggregate" && c.Retention.Mode != "delete" {
		return fmt.Errorf("field 'retention.mode' bernilai %q, harus salah satu dari: \"aggregate\" atau \"delete\"", c.Retention.Mode)
	}
	if c.Retention.RetainRawDays < 31 {
		return fmt.Errorf("field 'retention.retain_raw_days' bernilai %d, harus minimal 31 agar baseline bulanan tidak terhapus", c.Retention.RetainRawDays)
	}
	if c.WebServer.Port <= 0 || c.WebServer.Port > 65535 {
		return fmt.Errorf("field 'web_server.port' bernilai %d, harus antara 1-65535", c.WebServer.Port)
	}
	if c.DBPath == "" {
		return fmt.Errorf("field 'database_path' kosong - wajib diisi, contoh: \"C:\\\\ProgramData\\\\DiskHistMonitor\\\\diskhist.db\"")
	}
	if c.LogPath == "" {
		return fmt.Errorf("field 'log_path' kosong - wajib diisi, contoh: \"C:\\\\ProgramData\\\\DiskHistMonitor\\\\agent.log\"")
	}
	if c.Accuracy.VSSMode != "required" && c.Accuracy.VSSMode != "disabled" {
		return fmt.Errorf("field 'accuracy.vss_mode' bernilai %q, harus salah satu dari: \"required\" atau \"disabled\"", c.Accuracy.VSSMode)
	}
	if c.Resources.ScanTimeout <= 0 {
		return fmt.Errorf("field 'resources.scan_timeout' bernilai %s, harus lebih besar dari 0", c.Resources.ScanTimeout)
	}
	if c.Resources.CommandTimeout <= 0 {
		return fmt.Errorf("field 'resources.command_timeout' bernilai %s, harus lebih besar dari 0", c.Resources.CommandTimeout)
	}
	if c.Resources.CommandTimeout > c.Resources.ScanTimeout {
		return fmt.Errorf("field 'resources.command_timeout' (%s) tidak boleh melebihi 'resources.scan_timeout' (%s)", c.Resources.CommandTimeout, c.Resources.ScanTimeout)
	}
	if c.ConfigVersion != 1 {
		return fmt.Errorf("field 'config_version' bernilai %d, hanya versi 1 yang didukung", c.ConfigVersion)
	}
	if c.Ndelik.Enabled {
		if c.Ndelik.Server == "" {
			return fmt.Errorf("field 'ndelik.server' wajib diisi saat ndelik.enabled=true")
		}
		if c.Ndelik.AgentID == "" {
			return fmt.Errorf("field 'ndelik.agent_id' wajib diisi saat ndelik.enabled=true")
		}
		if c.Ndelik.APIKeyRef.Scheme == "" {
			return fmt.Errorf("field 'ndelik.api_key_ref' wajib diisi saat ndelik.enabled=true")
		}
	}
	return nil
}
