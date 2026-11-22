package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is what familiar needs to report. It lives in the user's config
// directory, and every field can also come from a FAMILIAR_* variable, which
// is how a container is set up.
type Config struct {
	URL       string `json:"url"`
	WebhookID string `json:"webhook_id"`
	Name      string `json:"name,omitempty"` // shown at the top of the menu

	IntervalSeconds int     `json:"interval_seconds,omitempty"`
	BusyCPU         float64 `json:"busy_cpu_percent,omitempty"`
	BusyGPU         float64 `json:"busy_gpu_percent,omitempty"`
	BusySamples     int     `json:"busy_samples,omitempty"`
	DiskPath        string  `json:"disk_path,omitempty"`
	// Disks lists the other drives to report. Empty: every local drive found.
	Disks []string `json:"disks,omitempty"`
	// AutoUpdate: install new releases by itself. On unless set to false.
	AutoUpdate *bool `json:"auto_update,omitempty"`
}

func (c Config) autoUpdate() bool { return c.AutoUpdate == nil || *c.AutoUpdate }

const (
	defaultInterval    = 30
	defaultBusyCPU     = 25
	defaultBusyGPU     = 20
	defaultBusySamples = 4 // with a 30 s interval, busy means busy for the last two minutes
)

func configPath() string {
	if p := os.Getenv("FAMILIAR_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "familiar", "config.json")
}

func loadConfig() (Config, error) {
	var cfg Config
	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("reading %s: %w", configPath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	applyEnv(&cfg, os.Getenv)
	return cfg, nil
}

func applyEnv(cfg *Config, getenv func(string) string) {
	if v := getenv("FAMILIAR_URL"); v != "" {
		cfg.URL = trimURL(v)
	}
	if v := getenv("FAMILIAR_WEBHOOK_ID"); v != "" {
		cfg.WebhookID = v
	}
	if v := getenv("FAMILIAR_NAME"); v != "" {
		cfg.Name = v
	}
	if v, err := strconv.Atoi(getenv("FAMILIAR_INTERVAL")); err == nil && v > 0 {
		cfg.IntervalSeconds = v
	}
	if v, err := strconv.ParseFloat(getenv("FAMILIAR_BUSY_CPU"), 64); err == nil && v > 0 {
		cfg.BusyCPU = v
	}
	if v, err := strconv.ParseFloat(getenv("FAMILIAR_BUSY_GPU"), 64); err == nil && v > 0 {
		cfg.BusyGPU = v
	}
	if v, err := strconv.Atoi(getenv("FAMILIAR_BUSY_SAMPLES")); err == nil && v > 0 {
		cfg.BusySamples = v
	}
	if v := getenv("FAMILIAR_DISK"); v != "" {
		cfg.DiskPath = v
	}
	if v := strings.ToLower(getenv("FAMILIAR_AUTO_UPDATE")); v != "" {
		on := v != "0" && v != "false" && v != "no" && v != "off"
		cfg.AutoUpdate = &on
	}
	if v := getenv("FAMILIAR_DISKS"); v != "" {
		cfg.Disks = nil
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.Disks = append(cfg.Disks, p)
			}
		}
	}
}

func withDefaults(cfg Config) Config {
	if cfg.IntervalSeconds <= 0 {
		cfg.IntervalSeconds = defaultInterval
	}
	if cfg.BusyCPU <= 0 {
		cfg.BusyCPU = defaultBusyCPU
	}
	if cfg.BusyGPU <= 0 {
		cfg.BusyGPU = defaultBusyGPU
	}
	if cfg.BusySamples <= 0 {
		cfg.BusySamples = defaultBusySamples
	}
	if cfg.DiskPath == "" {
		cfg.DiskPath = defaultDisk(runtime.GOOS)
	}
	if cfg.Name == "" {
		cfg.Name, _ = os.Hostname()
	}
	return cfg
}

// defaultDisk is the volume people mean by "my disk". On macOS that's the data
// volume: "/" is the small sealed system volume.
func defaultDisk(goos string) string {
	switch goos {
	case "windows":
		return `C:\`
	case "darwin":
		return "/System/Volumes/Data"
	default:
		return "/"
	}
}

func (c Config) validate() error {
	if c.URL == "" || c.WebhookID == "" {
		return fmt.Errorf("not set up yet: run familiar setup (settings file: %s)", configPath())
	}
	return nil
}

func (c Config) interval() time.Duration { return time.Duration(c.IntervalSeconds) * time.Second }

func saveConfig(cfg Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(path, append(data, '\n'), 0o600)
}

func trimURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }
