package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"miniav/pkg/config"
)

func TestConfigLoadsFixedWorkerTopology(t *testing.T) {
	base := t.TempDir()
	executable := filepath.Join(base, executableName("scanner"))
	signatures := filepath.Join(base, "signatures.txt")
	writeTestFile(t, executable, []byte("worker"), 0o700)
	writeTestFile(t, signatures, []byte("pattern"), 0o600)
	path := filepath.Join(base, "miniav.json")
	data := marshalFixedConfig(t, 1000, 500,
		workerConfig("scanner-a", filepath.Base(executable), filepath.Base(signatures)),
		workerConfig("scanner-b", filepath.Base(executable), filepath.Base(signatures)),
		workerConfig("scanner-c", filepath.Base(executable), filepath.Base(signatures)),
	)
	writeTestFile(t, path, data, 0o600)
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	workers := loaded.Workers()
	if len(workers) != 3 {
		t.Fatalf("worker count = %d, want 3", len(workers))
	}
	wantedTransports := []string{"stdio", "socket", "grpc"}
	for index, worker := range workers {
		if worker.Executable != executable || worker.SignaturePath != signatures {
			t.Fatalf("resolved worker %d = %#v", index, worker)
		}
		if worker.Transport != wantedTransports[index] {
			t.Fatalf("worker %d transport = %q, want %q", index, worker.Transport, wantedTransports[index])
		}
	}
}

func TestConfigRejectsInvalidFixedWorkerTopology(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "miniav.json")
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "old workers array", data: `{"startupTimeoutMs":1,"scanTimeoutMs":1,"workers":[]}`, want: "unknown field"},
		{name: "transport selection", data: `{"startupTimeoutMs":1,"scanTimeoutMs":1,"stdioWorker":{"transport":"grpc"}}`, want: "unknown field"},
		{name: "missing fixed workers", data: `{"startupTimeoutMs":1,"scanTimeoutMs":1}`, want: "stdioWorker.scannerId"},
		{name: "bad timeout", data: `{"startupTimeoutMs":0,"scanTimeoutMs":1}`, want: "greater than zero"},
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

func TestConfigRejectsDuplicateScannerAcrossFixedSlots(t *testing.T) {
	base := t.TempDir()
	executable := filepath.Join(base, executableName("scanner"))
	signatures := filepath.Join(base, "signatures.txt")
	writeTestFile(t, executable, []byte("worker"), 0o700)
	writeTestFile(t, signatures, []byte("pattern"), 0o600)
	path := filepath.Join(base, "miniav.json")
	worker := workerConfig("duplicate", executable, signatures)
	writeTestFile(t, path, marshalFixedConfig(t, 1, 1, worker, worker, workerConfig("scanner-c", executable, signatures)), 0o600)
	_, err := config.Load(path)
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("error = %v", err)
	}
}

func workerConfig(scannerID string, executable string, signatures string) map[string]any {
	return map[string]any{
		"scannerId": scannerID, "workerVersion": "1.0.0",
		"executable": executable, "signaturePath": signatures,
	}
}

func marshalFixedConfig(t *testing.T, startupTimeoutMs int, scanTimeoutMs int, stdioWorker map[string]any, socketWorker map[string]any, grpcWorker map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"startupTimeoutMs": startupTimeoutMs,
		"scanTimeoutMs":    scanTimeoutMs,
		"stdioWorker":      stdioWorker,
		"socketWorker":     socketWorker,
		"grpcWorker":       grpcWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
