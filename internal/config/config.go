package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config merepresentasikan seluruh isi config.yaml.
// Field ini harus selaras dengan PRD/SRS Bagian 2.5.
type Config struct {
	Drives    []string  `yaml:"drives"`
	MaxDepth  int       `yaml:"max_depth"`
	Schedule  string    `yaml:"schedule"`
	Retention Retention `yaml:"retention"`
	WebServer WebServer `yaml:"web_server"`
	Accuracy  Accuracy  `yaml:"accuracy"`
	Resources Resources `yaml:"resources"`
	DBPath    string    `yaml:"database_path"`
	LogPath   string    `yaml:"log_path"`
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
	ScanTimeout    time.Duration `yaml:"-"`
	CommandTimeout time.Duration `yaml:"-"`
	SingleScan     bool          `yaml:"single_scan"`
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
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config.yaml di %q tidak valid (syntax YAML error): %w", path, err)
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config.yaml di %q lolos parsing tapi nilainya tidak valid: %w", path, err)
	}
	return &cfg, nil
}

// applyDefaults mengisi nilai safe default untuk field accuracy/resources
// yang tidak ada di file config, agar perilaku aman (VSS required, resource
// bounds) berlaku tanpa perlu user mengubah config lama.
func (c *Config) applyDefaults() {
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
	c.Resources.SingleScan = true
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
	return nil
}
