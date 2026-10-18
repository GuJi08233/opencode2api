package config

import (
	"encoding/json"
	"os"
	"testing"
)

func TestRotatingProxyConfiguration(t *testing.T) {
	baseline, err := Load("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		attempts int
		fresh    bool
		valid    bool
	}{
		{name: "legacy defaults", valid: true},
		{name: "fresh connections only", fresh: true, valid: true},
		{name: "rotating gateway", attempts: 3, fresh: true, valid: true},
		{name: "negative retry budget", attempts: -1},
		{name: "retries require fresh connections", attempts: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseline
			cfg.Retry.AnonymousMaxAttempts = tc.attempts
			cfg.Performance.DisableProxyKeepAlives = tc.fresh
			cfg, err := Normalize("config.json", cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%t, error=%v", tc.valid, err)
			}
			if tc.valid {
				path := t.TempDir() + "/config.json"
				data, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				saved, err := Load(path)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Retry.AnonymousMaxAttempts != tc.attempts || saved.Performance.DisableProxyKeepAlives != tc.fresh {
					t.Fatal("rotating proxy settings did not survive save/reload")
				}
			}
		})
	}
}
