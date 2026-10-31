package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Valid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
version: 1
defaults:
  provider: main
  max_retries: 5
  verbose: true
providers:
  - name: main
    host: news.example.com
    port: 563
    encryption: true
    username: user
    password: pass
    connections: 8
    priority: 1
  - name: backup
    host: news2.example.com
    port: 119
    encryption: false
    username: user2
    password: pass2
    connections: 4
    priority: 2
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 {
		t.Fatalf("expected version 1, got %d", cfg.Version)
	}
	if cfg.Defaults.Provider != "main" {
		t.Fatalf("expected default provider 'main', got %q", cfg.Defaults.Provider)
	}
	if cfg.Defaults.MaxRetries != 5 {
		t.Fatalf("expected max_retries 5, got %d", cfg.Defaults.MaxRetries)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("expected 2 providers, got %d", len(cfg.Providers))
	}
	if cfg.Providers[0].Name != "main" || cfg.Providers[1].Name != "backup" {
		t.Fatalf("unexpected provider names: %s, %s", cfg.Providers[0].Name, cfg.Providers[1].Name)
	}
	if !cfg.Providers[0].Encryption {
		t.Fatal("expected main provider to use encryption")
	}
}

func TestLoad_EnvExpansion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
version: 1
providers:
  - name: test
    host: news.example.com
    port: 563
    username: $(NZB_TEST_USER)
    password: ${NZB_TEST_PW}
    connections: 4
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NZB_TEST_USER", "realuser")
	t.Setenv("NZB_TEST_PW", "secret-from-env")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers[0].Password != "secret-from-env" {
		t.Fatalf("password expansion: got %q", cfg.Providers[0].Password)
	}
	if cfg.Providers[0].Username != "realuser" {
		t.Fatalf("$(VAR) username expansion: got %q", cfg.Providers[0].Username)
	}
}

func TestLoad_EnvUnresolvedLeftIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
version: 1
providers:
  - name: test
    host: news.example.com
    port: 563
    username: $(DOES_NOT_EXIST_XYZ)
    password: literal-pw
    connections: 4
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Unset var is left as the literal token (so the failure is visible).
	if cfg.Providers[0].Username != "$(DOES_NOT_EXIST_XYZ)" {
		t.Fatalf("expected literal token preserved, got %q", cfg.Providers[0].Username)
	}
	if cfg.Providers[0].Password != "literal-pw" {
		t.Fatalf("literal password should be unchanged, got %q", cfg.Providers[0].Password)
	}
}

func TestActiveProviders_Default(t *testing.T) {
	cfg := &Config{
		Version:  1,
		Defaults: Defaults{Provider: "main"},
		Providers: []Provider{
			{Name: "backup", Priority: 2},
			{Name: "main", Priority: 1},
		},
	}
	active := ActiveProviders(cfg, "")
	if len(active) != 1 {
		t.Fatalf("expected 1 active, got %d", len(active))
	}
	if active[0].Name != "main" {
		t.Fatalf("expected 'main', got %q", active[0].Name)
	}
}

func TestActiveProviders_Override(t *testing.T) {
	cfg := &Config{
		Version:  1,
		Defaults: Defaults{Provider: "main"},
		Providers: []Provider{
			{Name: "main", Priority: 1},
			{Name: "backup", Priority: 2},
		},
	}
	active := ActiveProviders(cfg, "backup")
	if len(active) != 1 || active[0].Name != "backup" {
		t.Fatalf("expected 'backup', got %+v", active)
	}
}

func TestEnsureDefault_CreatesWhenMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "dir", "config.yaml")
	created, err := EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("expected created=true for missing config")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("default config not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("expected non-empty default config")
	}
	// The written template must itself be loadable and valid.
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("default template failed to load: %v", err)
	}
	if len(cfg.Providers) == 0 {
		t.Fatal("default template defines no providers")
	}
}

func TestEnsureDefault_LeavesExistingAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	existing := "version: 1\nproviders:\n  - name: keep\n    host: h\n    port: 1\n"
	if err := os.WriteFile(path, []byte(existing), 0644); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureDefault(path)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("expected created=false when file already exists")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Fatal("existing config was overwritten")
	}
}
