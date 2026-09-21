package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"miniav/pkg/config"
)

func TestConfigLoadsAndResolvesWorkers(t *testing.T) {
	base := t.TempDir()
	executable := filepath.Join(base, executableName("scanner-a"))
	signatures := filepath.Join(base, "signatures.txt")
	writeTestFile(t, executable, []byte("worker"), 0o700)
	writeTestFile(t, signatures, []byte("pattern"), 0o600)
	path := filepath.Join(base, "miniav.json")
	data, err := json.Marshal(map[string]any{
		"startupTimeoutMs": 1000,
		"scanTimeoutMs":    500,
		"workers": []map[string]any{{
			"scannerId": "scanner-a", "workerVersion": "1.0.0",
			"executable": filepath.Base(executable), "signaturePath": filepath.Base(signatures),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, data, 0o600)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workers[0].Executable != executable || loaded.Workers[0].SignaturePath != signatures {
		t.Fatalf("resolved worker = %#v", loaded.Workers[0])
	}
}

func TestConfigRejectsInvalidInput(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "miniav.json")
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "unknown", data: `{"startupTimeoutMs":1,"scanTimeoutMs":1,"workers":[],"other":true}`, want: "unknown field"},
		{name: "no workers", data: `{"startupTimeoutMs":1,"scanTimeoutMs":1,"workers":[]}`, want: "at least one"},
		{name: "bad timeout", data: `{"startupTimeoutMs":0,"scanTimeoutMs":1,"workers":[]}`, want: "greater than zero"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(test.data), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := config.Load(path)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
