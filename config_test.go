package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig_Missing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if cfg != (Config{}) {
		t.Fatalf("expected zero-value Config, got %+v", cfg)
	}
}

func TestSaveConfig_CreatesDirAndFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barfi")
	path := filepath.Join(dir, "config.json")
	cfg := Config{
		Server:     "https://bus.example.com",
		Token:      "tok",
		LocationId: "loc",
		ParentId:   "par",
		Workers:    5,
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v, want 0700", di.Mode().Perm())
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode = %v, want 0600", fi.Mode().Perm())
	}
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if loaded != cfg {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", loaded, cfg)
	}
}

func TestSaveConfig_OmitEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "barfi")
	path := filepath.Join(dir, "config.json")
	cfg := Config{Server: "https://bus.example.com"}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	for _, forbidden := range []string{`"token"`, `"locationId"`, `"parentId"`, `"workers"`} {
		if strings.Contains(got, forbidden) {
			t.Errorf("JSON should omit %s on empty value, got: %s", forbidden, got)
		}
	}
}

func TestSaveConfig_PreservesExistingFieldsViaResolve(t *testing.T) {
	// Simulates: existing config with server+token, user runs
	// `barfi --save --parent-id abc`. Expected: all three persist afterward.
	dir := filepath.Join(t.TempDir(), "barfi")
	path := filepath.Join(dir, "config.json")

	// 1. Seed the file with an existing server+token.
	if err := saveConfig(path, Config{Server: "https://a", Token: "tok"}); err != nil {
		t.Fatal(err)
	}

	// 2. Load, merge flag override, save again.
	loaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded.ParentId = "abc"
	if err := saveConfig(path, loaded); err != nil {
		t.Fatal(err)
	}

	// 3. Verify all three fields are present.
	final, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.Server != "https://a" {
		t.Errorf("Server = %q, want %q", final.Server, "https://a")
	}
	if final.Token != "tok" {
		t.Errorf("Token = %q, want %q", final.Token, "tok")
	}
	if final.ParentId != "abc" {
		t.Errorf("ParentId = %q, want %q", final.ParentId, "abc")
	}
}
