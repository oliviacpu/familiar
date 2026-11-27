package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Config is what familiar needs to report. It lives in the user's config
// directory, and every field can also come from a FAMILIAR_* variable, which
// is how a container is set up.
type Config struct {
	URL       string `json:"url,omitempty"`
	WebhookID string `json:"webhook_id,omitempty"`
	Name      string `json:"name,omitempty"` // shown at the top of the menu

	// MQTT, used instead of the webhook when MQTTURL is set (mqtt.go).
	MQTTURL      string `json:"mqtt_url,omitempty"`  // tcp://host:1883
	MQTTUser     string `json:"mqtt_user,omitempty"` // the device ID unless set
	MQTTPassword secret `json:"mqtt_password,omitempty"`
	// MQTTPasswordFile is read for the password at start, e.g. a container's secret file.
	MQTTPasswordFile string `json:"mqtt_password_file,omitempty"`
	// DeviceID names this computer in its MQTT topics. A slug of Name unless set.
	DeviceID string `json:"device_id,omitempty"`
	// EntityPrefix starts each entity ID (sensor.<prefix>_cpu). A slug of Name
	// unless set; set it to keep the entity IDs a computer already has.
	EntityPrefix string `json:"entity_prefix,omitempty"`

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

// secret is a password. Printing it (or a Config holding it, with %v or %+v)
// shows a placeholder; JSON still saves it.
type secret string

func (s secret) String() string {
	if s == "" {
		return ""
	}
	return "…"
}

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
	cfg, err := readConfigFile()
	if err != nil {
		return cfg, err
	}
	applyEnv(&cfg, os.Getenv)
	return cfg, readPasswordFile(&cfg)
}

// readConfigFile is the settings file alone, without FAMILIAR_* variables:
// what setup changes and saves again. No file is no settings.
func readConfigFile() (Config, error) {
	var cfg Config
	data, err := os.ReadFile(configPath())
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("reading %s: %w", configPath(), err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	return cfg, nil
}

// readPasswordFile takes the MQTT password from its file, when there is one.
// A file wins over a password set elsewhere: it's the deliberate choice in a
// container. Only its trailing line break is dropped.
func readPasswordFile(cfg *Config) error {
	if cfg.MQTTPasswordFile == "" {
		return nil
	}
	data, err := os.ReadFile(cfg.MQTTPasswordFile)
	if err != nil {
		return fmt.Errorf("reading the MQTT password file: %w", err)
	}
	cfg.MQTTPassword = secret(strings.TrimRight(string(data), "\r\n"))
	return nil
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
	if v := getenv("FAMILIAR_MQTT_URL"); v != "" {
		cfg.MQTTURL = strings.TrimSpace(v)
	}
	if v := getenv("FAMILIAR_MQTT_USER"); v != "" {
		cfg.MQTTUser = v
	}
	if v := getenv("FAMILIAR_MQTT_PASSWORD"); v != "" {
		cfg.MQTTPassword = secret(v)
	}
	if v := getenv("FAMILIAR_MQTT_PASSWORD_FILE"); v != "" {
		cfg.MQTTPasswordFile = v
	}
	if v := getenv("FAMILIAR_DEVICE_ID"); v != "" {
		cfg.DeviceID = v
	}
	if v := getenv("FAMILIAR_ENTITY_PREFIX"); v != "" {
		cfg.EntityPrefix = v
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
	if cfg.DeviceID == "" {
		cfg.DeviceID = nameSlug(cfg.Name)
	}
	if cfg.EntityPrefix = slug(cfg.EntityPrefix); cfg.EntityPrefix == "" {
		cfg.EntityPrefix = nameSlug(cfg.Name)
	}
	if cfg.MQTTUser == "" {
		cfg.MQTTUser = cfg.DeviceID // so a broker ACL can give each user the topics named after it
	}
	return cfg
}

// nameSlug is a computer's name as an ID: "Office PC" is office_pc. A name
// with nothing slug keeps (another script, say) becomes a hash of itself.
func nameSlug(name string) string {
	if s := slug(name); s != "" {
		return s
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return fmt.Sprintf("%08x", h.Sum32())
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
	if c.MQTTURL != "" {
		if err := checkMQTTURL(c.MQTTURL); err != nil {
			return err
		}
		if !topicID.MatchString(c.DeviceID) {
			return fmt.Errorf("device_id %q may only have letters, digits, _ and -", c.DeviceID)
		}
		return nil
	}
	if c.URL == "" || c.WebhookID == "" {
		return fmt.Errorf("not set up yet: run familiar setup (settings file: %s)", configPath())
	}
	return nil
}

// topicID is what Home Assistant allows in a discovery topic's ID.
var topicID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// checkMQTTURL checks a broker address. Its errors leave the address out, in
// case it holds a password.
func checkMQTTURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("mqtt_url should look like tcp://homeassistant.local:1883")
	}
	if u.User != nil {
		return errors.New("put the MQTT user and password in mqtt_user and mqtt_password, not in mqtt_url")
	}
	switch u.Scheme {
	case "tcp", "mqtt", "ssl", "tls", "mqtts":
		if u.Port() == "" {
			return errors.New("mqtt_url needs a port, e.g. tcp://homeassistant.local:1883 (8883 for ssl://)")
		}
	case "ws", "wss":
	default:
		return fmt.Errorf("mqtt_url: unknown scheme %q (tcp, ssl, ws or wss)", u.Scheme)
	}
	return nil
}

// userWarning is set when the MQTT user isn't the device ID. A broker whose
// ACL gives each user the topics named after it (familiar/%u/#) then refuses
// this computer's.
func (c Config) userWarning() string {
	if c.MQTTURL == "" || c.MQTTUser == c.DeviceID {
		return ""
	}
	return fmt.Sprintf("the MQTT user %q isn't the device ID %q; a broker that only lets each user write its own topics (familiar/<user>/…) will refuse this computer's", c.MQTTUser, c.DeviceID)
}

// destination is where readings go, for the log.
func (c Config) destination() string {
	if c.MQTTURL != "" {
		return c.MQTTURL
	}
	return c.URL
}

func (c Config) interval() time.Duration { return time.Duration(c.IntervalSeconds) * time.Second }

func saveConfig(cfg Config) error {
	path := configPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	// A new file, 0600 from the start (CreateTemp's mode), renamed over the
	// old one: the password and webhook ID are never in a file that others
	// can read, even for a moment.
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func trimURL(u string) string { return strings.TrimRight(strings.TrimSpace(u), "/") }
