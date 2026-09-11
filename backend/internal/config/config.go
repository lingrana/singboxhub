// Package config loads panel configuration from (in order of precedence)
// command-line flags, environment variables, an optional YAML file, and
// built-in defaults.
package config

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the resolved runtime configuration of the panel.
type Config struct {
	ListenAddr string        `yaml:"listen_addr"`
	DataDir    string        `yaml:"data_dir"`
	DBPath     string        `yaml:"db_path"`
	DBDriver   string        `yaml:"db_driver"`
	DBDSN      string        `yaml:"db_dsn"`
	LogLevel   string        `yaml:"log_level"`
	Session    SessionConfig `yaml:"session"`
	Probe      ProbeConfig   `yaml:"probe"`
	Sampler    SamplerConfig `yaml:"sampler"`
}

// DatabaseSelection is the persisted first-run database choice.
type DatabaseSelection struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"`
}

// DatabaseSelectionPath returns the file used to remember the selected store.
func DatabaseSelectionPath(dataDir string) string {
	return filepath.Join(dataDir, "database.json")
}

// LoadDatabaseSelection loads the first-run database choice.
func LoadDatabaseSelection(dataDir string) (DatabaseSelection, bool, error) {
	raw, err := os.ReadFile(DatabaseSelectionPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return DatabaseSelection{}, false, nil
	}
	if err != nil {
		return DatabaseSelection{}, false, fmt.Errorf("read database selection: %w", err)
	}
	var selection DatabaseSelection
	if err := json.Unmarshal(raw, &selection); err != nil {
		return DatabaseSelection{}, false, fmt.Errorf("parse database selection: %w", err)
	}
	if err := ValidateDatabaseSelection(selection); err != nil {
		return DatabaseSelection{}, false, err
	}
	return selection, true, nil
}

// SaveDatabaseSelection atomically persists a validated database choice.
func SaveDatabaseSelection(dataDir string, selection DatabaseSelection) error {
	if err := ValidateDatabaseSelection(selection); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	raw, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(dataDir, ".database-*.json")
	if err != nil {
		return fmt.Errorf("create database selection: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, DatabaseSelectionPath(dataDir)); err != nil {
		return fmt.Errorf("store database selection: %w", err)
	}
	return nil
}

// ValidateDatabaseSelection validates the user-provided driver and DSN.
func ValidateDatabaseSelection(selection DatabaseSelection) error {
	selection.Driver = strings.ToLower(strings.TrimSpace(selection.Driver))
	selection.DSN = strings.TrimSpace(selection.DSN)
	if selection.Driver != "sqlite" && selection.Driver != "postgres" {
		return errors.New("database driver must be sqlite or postgres")
	}
	if selection.DSN == "" || strings.ContainsAny(selection.DSN, "\r\n") {
		return errors.New("database dsn is required and must be one line")
	}
	return nil
}

// SessionConfig controls authentication token lifetimes.
type SessionConfig struct {
	AccessTokenTTL  time.Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL time.Duration `yaml:"refresh_token_ttl"`
}

// ProbeConfig configures the exit-IP probing feature.
type ProbeConfig struct {
	SingboxBinPath      string `yaml:"singbox_bin_path"`
	IPProviderURL       string `yaml:"ip_provider_url"`
	ProbeTimeoutSeconds int    `yaml:"probe_timeout_seconds"`
}

// SamplerConfig configures the traffic sampling defaults.
type SamplerConfig struct {
	TrafficInterval time.Duration `yaml:"traffic_interval"`
	ConnInterval    time.Duration `yaml:"conn_interval"`
	PersistInterval time.Duration `yaml:"persist_interval"`
	RetentionDays   int           `yaml:"retention_days"`
}

func defaults() Config {
	return Config{
		ListenAddr: ":9090",
		DataDir:    "data",
		DBPath:     "",
		LogLevel:   "info",
		Session: SessionConfig{
			AccessTokenTTL:  30 * time.Minute,
			RefreshTokenTTL: 30 * 24 * time.Hour,
		},
		Probe: ProbeConfig{
			IPProviderURL:       "http://ip-api.com/json/?fields=query,country,city,as",
			ProbeTimeoutSeconds: 15,
		},
		Sampler: SamplerConfig{
			TrafficInterval: time.Second,
			ConnInterval:    2 * time.Second,
			PersistInterval: 10 * time.Second,
			RetentionDays:   30,
		},
	}
}

// Load parses the -config flag (optional YAML file), then applies environment
// overrides, then flags. DBPath defaults to <DataDir>/panel.db.
func Load(args []string) (Config, error) {
	cfg := defaults()

	var (
		configPath = flag.String("config", "", "path to YAML config file")
		listen     = flag.String("listen", "", "listen address (env SINGHUB_LISTEN)")
		dataDir    = flag.String("data-dir", "", "data directory (env SINGHUB_DATA_DIR)")
		logLevel   = flag.String("log-level", "", "log level: debug|info|warn|error")
	)
	flag.CommandLine.Parse(args)

	if *configPath != "" {
		raw, err := os.ReadFile(*configPath)
		if err != nil {
			return cfg, fmt.Errorf("read config file: %w", err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config file: %w", err)
		}
	}

	applyEnv(&cfg)
	if *listen != "" {
		cfg.ListenAddr = *listen
	}
	if *dataDir != "" {
		cfg.DataDir = *dataDir
	}
	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join(cfg.DataDir, "panel.db")
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	setStr := func(dst *string, key string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	setStr(&cfg.ListenAddr, "SINGHUB_LISTEN")
	setStr(&cfg.DataDir, "SINGHUB_DATA_DIR")
	setStr(&cfg.DBPath, "SINGHUB_DB_PATH")
	setStr(&cfg.DBDriver, "SINGHUB_DB_DRIVER")
	setStr(&cfg.DBDSN, "SINGHUB_DB_DSN")
	setStr(&cfg.LogLevel, "SINGHUB_LOG_LEVEL")
	setStr(&cfg.Probe.SingboxBinPath, "SINGHUB_SINGBOX_BIN")
	setStr(&cfg.Probe.IPProviderURL, "SINGHUB_IP_PROVIDER_URL")
}

func (c Config) validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.Session.AccessTokenTTL <= 0 || c.Session.RefreshTokenTTL <= 0 {
		return errors.New("session token TTLs must be positive")
	}
	if c.Sampler.TrafficInterval <= 0 || c.Sampler.ConnInterval <= 0 || c.Sampler.PersistInterval <= 0 {
		return errors.New("sampler intervals must be positive")
	}
	if c.Sampler.RetentionDays <= 0 {
		return errors.New("retention_days must be positive")
	}
	return nil
}
