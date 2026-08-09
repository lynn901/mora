package config

import (
	"os"
	"testing"
)

// helper: set env, return cleanup. os.Setenv is safe in tests because the
// process environment is the only source for these keys.
func setEnv(t *testing.T, key, val string) {
	t.Helper()
	old, ok := os.LookupEnv(key)
	if err := os.Setenv(key, val); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, old)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

// TestLoadMoraAPIURL covers the mora-api Load() path: MORA_API_URL is honored,
// trailing slash trimmed is not required here (Load does not trim), and the
// default applies when unset. This guards against the YS-48 regression where
// config read the old WIKI_API_URL key and silently fell back to localhost.
func TestLoadMoraAPIURL(t *testing.T) {
	t.Run("honors MORA_API_URL", func(t *testing.T) {
		setEnv(t, "MORA_API_URL", "http://mora-api:8080")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MoraAPIURL != "http://mora-api:8080" {
			t.Fatalf("MoraAPIURL = %q, want http://mora-api:8080", cfg.MoraAPIURL)
		}
	})

	t.Run("defaults to localhost when unset", func(t *testing.T) {
		// Ensure neither the new nor the legacy key leaks in.
		setEnv(t, "MORA_API_URL", "")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MoraAPIURL != "http://localhost:8080" {
			t.Fatalf("MoraAPIURL = %q, want default http://localhost:8080", cfg.MoraAPIURL)
		}
	})

	t.Run("ignores legacy WIKI_API_URL", func(t *testing.T) {
		// Legacy alias must NOT be read anymore (clean switch, design-docs/08 §4-D).
		setEnv(t, "MORA_API_URL", "")
		setEnv(t, "WIKI_API_URL", "http://legacy:9999")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.MoraAPIURL != "http://localhost:8080" {
			t.Fatalf("MoraAPIURL = %q; legacy WIKI_API_URL must not be read", cfg.MoraAPIURL)
		}
	})
}

// TestFromEnvMoraAPIURL covers the mcp-server FromEnv() path (the actual
// runtime used by get_document): it honors MORA_API_URL, trims a trailing
// slash, and ignores the legacy WIKI_API_URL key.
func TestFromEnvMoraAPIURL(t *testing.T) {
	t.Run("honors MORA_API_URL and trims trailing slash", func(t *testing.T) {
		setEnv(t, "MORA_API_URL", "http://mora-api:8080/")
		cfg := FromEnv()
		if cfg.MoraAPIURL != "http://mora-api:8080" {
			t.Fatalf("MoraAPIURL = %q, want http://mora-api:8080", cfg.MoraAPIURL)
		}
	})

	t.Run("ignores legacy WIKI_API_URL", func(t *testing.T) {
		setEnv(t, "MORA_API_URL", "")
		setEnv(t, "WIKI_API_URL", "http://legacy:9999")
		cfg := FromEnv()
		if cfg.MoraAPIURL != "http://localhost:8080" {
			t.Fatalf("MoraAPIURL = %q; legacy WIKI_API_URL must not be read in FromEnv", cfg.MoraAPIURL)
		}
	})
}
