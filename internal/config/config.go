package config

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// defaultConfigYAML is the template written to the user's config path the first
// time gimme runs and finds no config there. It is a valid, commented example so
// the user has something to edit instead of a "no such file or directory" error.
//
//go:embed default.yaml
var defaultConfigYAML []byte

// Default returns the embedded default config template bytes.
func Default() []byte { return defaultConfigYAML }

// EnsureDefault writes the default config template to path if no file exists
// there yet, creating the parent directory as needed. It reports created=true
// when a file was written. If the file already exists it is left untouched and
// created=false is returned. This lets a first run "just work" instead of
// failing with a missing-file error.
func EnsureDefault(path string) (created bool, err error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil // already present
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat config: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("create config dir: %w", err)
	}
	// 0600: the template references env vars rather than literal secrets, but the
	// user may paste real credentials into it, so keep it owner-only by default.
	if err := os.WriteFile(path, defaultConfigYAML, 0o600); err != nil {
		return false, fmt.Errorf("write default config: %w", err)
	}
	return true, nil
}

// CurrentVersion is the config schema version this binary understands. Version 2
// introduced loading config from ~/.config/gimme/config.yaml by default (older
// version-1 files are still accepted; the schema is otherwise unchanged).
const CurrentVersion = 2

// Config is the top-level configuration loaded from config.yaml.
type Config struct {
	Version   int        `yaml:"version"`
	Defaults  Defaults   `yaml:"defaults"`
	Providers []Provider `yaml:"providers"`
}

// Defaults holds global defaults.
type Defaults struct {
	Provider   string `yaml:"provider"`    // default provider name
	MaxRetries int    `yaml:"max_retries"` // retries per segment before marking as failed
	Verbose    bool   `yaml:"verbose"`     // verbose logging enabled
}

// Provider describes one NNTP server connection.
type Provider struct {
	Name        string `yaml:"name"`
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	Encryption  bool   `yaml:"encryption"`
	Username    string `yaml:"username"`
	Password    string `yaml:"password"`
	Connections int    `yaml:"connections"`
	Priority    int    `yaml:"priority"` // lower = tried first; 0 means same as first
}

// Load reads and validates the config from the given path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expand ${VAR} and $(VAR) references in username/password from environment.
	expandProviderVars(&cfg)

	if err := Validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate checks that the config is usable.
func Validate(cfg *Config) error {
	if cfg.Version == 0 {
		return fmt.Errorf("config missing 'version' field")
	}
	if cfg.Version > CurrentVersion {
		return fmt.Errorf("config version %d is newer than supported %d", cfg.Version, CurrentVersion)
	}
	if len(cfg.Providers) == 0 {
		return fmt.Errorf("config must define at least one provider")
	}
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider[%d]: 'name' is required", i)
		}
		if p.Host == "" {
			return fmt.Errorf("provider[%d] (%s): 'host' is required", i, p.Name)
		}
		if p.Port == 0 {
			return fmt.Errorf("provider[%d] (%s): 'port' is required", i, p.Name)
		}
		if p.Connections <= 0 {
			cfg.Providers[i].Connections = 4 // default
		}
		// Trim accidental surrounding whitespace (YAML/env expansion safety).
		cfg.Providers[i].Name = strings.TrimSpace(cfg.Providers[i].Name)
		cfg.Providers[i].Host = strings.TrimSpace(cfg.Providers[i].Host)
		cfg.Providers[i].Username = strings.TrimSpace(cfg.Providers[i].Username)
		cfg.Providers[i].Password = strings.TrimSpace(cfg.Providers[i].Password)
	}
	if cfg.Defaults.MaxRetries <= 0 {
		cfg.Defaults.MaxRetries = 3
	}
	return nil
}

// ActiveProviders returns providers sorted by priority (lower first).
// If override is non-empty, only providers with that name are returned.
func ActiveProviders(cfg *Config, override string) []Provider {
	var active []Provider
	for _, p := range cfg.Providers {
		if override != "" {
			if p.Name == override {
				active = append(active, p)
			}
		} else if cfg.Defaults.Provider != "" {
			if p.Name == cfg.Defaults.Provider {
				active = append(active, p)
			}
		} else {
			active = append(active, p)
		}
	}
	sort.Slice(active, func(i, j int) bool {
		return active[i].Priority < active[j].Priority
	})
	return active
}

// envBraceRe matches ${VAR_NAME}; envParenRe matches $(VAR_NAME). Both are
// expanded from the environment so config authors can use either style.
var (
	envBraceRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)
	envParenRe = regexp.MustCompile(`\$\(([A-Za-z_][A-Za-z0-9_]*)\)`)
)

// expandProviderVars expands ${VAR}/$(VAR) in the username and password of
// every provider. Unresolved references are left untouched so the failure is
// visible rather than silently sending an empty credential.
func expandProviderVars(cfg *Config) {
	for i := range cfg.Providers {
		cfg.Providers[i].Username = expandEnv(cfg.Providers[i].Username)
		cfg.Providers[i].Password = expandEnv(cfg.Providers[i].Password)
	}
}

func expandEnv(s string) string {
	s = envBraceRe.ReplaceAllStringFunc(s, func(m string) string {
		return lookupEnv(envBraceRe.FindStringSubmatch(m)[1], m)
	})
	s = envParenRe.ReplaceAllStringFunc(s, func(m string) string {
		return lookupEnv(envParenRe.FindStringSubmatch(m)[1], m)
	})
	return s
}

func lookupEnv(name, fallback string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return fallback // leave $(NAME) as-is so it's obvious the env var is unset
}
