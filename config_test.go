package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRejectsUnsafeOrAmbiguousSettings(t *testing.T) {
	data, err := os.ReadFile("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	valid := string(data)
	for _, bad := range []string{
		strings.Replace(valid, `"max_resets": 3`, `"max_resets": 4`, 1),
		strings.Replace(valid, `"interval": "10s"`, `"interval": "0s"`, 1),
		strings.Replace(valid, `"max_loss": 0.5`, `"max_loss": 1`, 1),
		strings.Replace(valid, `"interface"`, `"interafce"`, 1),
		strings.Replace(valid, "replace-with-fallback-profile-uuid", "replace-with-primary-profile-uuid", 1),
		strings.Replace(valid, "1.1.1.1", "192.168.0.1", 1),
		valid + "{}",
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(path); err == nil {
			t.Fatalf("accepted invalid configuration: %s", bad)
		}
	}
	if _, err := loadConfig("config.example.json"); err != nil {
		t.Fatal(err)
	}
}
