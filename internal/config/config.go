package config

import (
	"fmt"
	"os"

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
	DBPath    string    `yaml:"database_path"`
	LogPath   string    `yaml:"log_path"`
}

type Retention struct {
	Mode           string `yaml:"mode"` // "aggregate" atau "delete"
	RetainRawDays  int    `yaml:"retain_raw_days"`
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

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config.yaml di %q lolos parsing tapi nilainya tidak valid: %w", path, err)
	}

	return &cfg, nil
}

// validate memastikan nilai-nilai config masuk akal.
// Sesuai NFR-05: TIDAK boleh diam-diam pakai default jika nilai mencurigakan -
// harus return error spesifik supaya user tahu persis apa yang perlu diperbaiki.
func (c *Config) validate() error {
	if len(c.Drives) == 0 {
		return fmt.Errorf("field 'drives' kosong - minimal harus ada 1 drive yang dipantau, contoh: drives: [\"D:\\\\\"]")
	}
	if c.MaxDepth <= 0 {
		return fmt.Errorf("field 'max_depth' bernilai %d, harus lebih besar dari 0", c.MaxDepth)
	}
	if c.Retention.Mode != "aggregate" && c.Retention.Mode != "delete" {
		return fmt.Errorf("field 'retention.mode' bernilai %q, harus salah satu dari: \"aggregate\" atau \"delete\"", c.Retention.Mode)
	}
	if c.Retention.RetainRawDays <= 0 {
		return fmt.Errorf("field 'retention.retain_raw_days' bernilai %d, harus lebih besar dari 0", c.Retention.RetainRawDays)
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
	return nil
}
