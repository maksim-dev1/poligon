// Package config loads poligon's static configuration: server settings and the
// device inventory (config/devices.yaml).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pancir/poligon/internal/model"
)

// Config is the merged runtime configuration.
type Config struct {
	Listen        string        `yaml:"listen"`         // e.g. ":8080"
	DBPath        string        `yaml:"db_path"`        // sqlite file
	StorageDir    string        `yaml:"storage_dir"`    // artifacts + history
	PollInterval  time.Duration `yaml:"poll_interval"`  // device health poll
	SpecsInterval time.Duration `yaml:"specs_interval"` // specs refresh
	IdleTimeout   time.Duration `yaml:"idle_timeout"`   // auto-release after no heartbeat
	MaxLease      time.Duration `yaml:"max_lease"`      // hard cap on a reservation
	ADBPath       string        `yaml:"adb_path"`
	AutoDiscover  bool          `yaml:"auto_discover"` // register unknown devices on connect
	LiveSidecar   string        `yaml:"live_sidecar"`  // ws-scrcpy base URL, "" disables live screen
	Devices       []DeviceSpec  `yaml:"devices"`
}

// DeviceSpec is one entry in devices.yaml.
type DeviceSpec struct {
	ID       string         `yaml:"id"`
	Platform model.Platform `yaml:"platform"`
	Serial   string         `yaml:"serial"`
	UDID     string         `yaml:"udid"`
	Tags     []string       `yaml:"tags"`
}

// Default returns config with sane defaults applied.
func Default() Config {
	return Config{
		Listen:        ":8080",
		DBPath:        "poligon.db",
		StorageDir:    "storage",
		PollInterval:  10 * time.Second,
		SpecsInterval: time.Hour,
		IdleTimeout:   15 * time.Minute,
		MaxLease:      4 * time.Hour,
		ADBPath:       "adb",
		AutoDiscover:  true,
		LiveSidecar:   "http://127.0.0.1:8000",
	}
}

// Load reads a YAML config file and overlays it onto the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	seen := map[string]bool{}
	for _, d := range c.Devices {
		if d.ID == "" {
			return fmt.Errorf("device with empty id")
		}
		if seen[d.ID] {
			return fmt.Errorf("duplicate device id %q", d.ID)
		}
		seen[d.ID] = true
		switch d.Platform {
		case model.Android:
			if d.Serial == "" {
				return fmt.Errorf("device %q: android needs serial", d.ID)
			}
		case model.IOS:
			if d.UDID == "" {
				return fmt.Errorf("device %q: ios needs udid", d.ID)
			}
		default:
			return fmt.Errorf("device %q: bad platform %q", d.ID, d.Platform)
		}
	}
	return nil
}
