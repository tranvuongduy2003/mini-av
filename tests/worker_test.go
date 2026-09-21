package tests

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"miniav/pkg/ipc"
	"miniav/pkg/protocol"
	"miniav/pkg/worker"
)

func TestWorkerProtocolLifecycle(t *testing.T) {
	signaturesPath := writeSignatureDatabase(t, "first-pattern\n")
	malwarePath := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(malwarePath, []byte("contains first-pattern"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanPath := filepath.Join(t.TempDir(), "clean.txt")
	if err := os.WriteFile(cleanPath, []byte("clean content"), 0o600); err != nil {
		t.Fatal(err)
	}

	inputReader, inputWriter := workerPipe(t)
	outputReader, outputWriter := workerPipe(t)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(context.Background(), worker.Config{ScannerID: "scanner-a", WorkerVersion: "1.0.0", SignaturePath: signaturesPath}, inputReader, outputWriter)
	}()
	encoder := ipc.NewEncoder(inputWriter)
	decoder := ipc.NewDecoder(outputReader)

	requests := []protocol.Message{
		{Version: protocol.Version, Type: protocol.TypeHello},
		{Version: protocol.Version, Type: protocol.TypeHealthCheck},
		{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 1, FilePath: malwarePath},
		{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 2, FilePath: cleanPath},
	}
	for _, request := range requests {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
	}

	hello, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if hello.Type != protocol.TypeHelloAck || hello.ScannerID != "scanner-a" || hello.WorkerVersion != "1.0.0" {
		t.Fatalf("hello response = %#v", hello)
	}
	health, err := decoder.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if health.Type != protocol.TypeHealthResult || health.Status != "ok" {
		t.Fatalf("health response = %#v", health)
	}
	results := map[uint64]protocol.Message{}
	for len(results) < 2 {
		response, decodeErr := decoder.Decode()
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		results[response.RequestID] = response
	}
	if results[1].Verdict != protocol.VerdictMalware || results[1].Signature != "first-pattern" {
		t.Fatalf("malware result = %#v", results[1])
	}
	if results[2].Verdict != protocol.VerdictClean {
		t.Fatalf("clean result = %#v", results[2])
	}

	if err := encoder.Encode(protocol.Message{Version: protocol.Version, Type: protocol.TypeShutdown}); err != nil {
		t.Fatal(err)
	}
	shutdown, err := decoder.Decode()
	if err != nil || shutdown.Type != protocol.TypeShutdownAck {
		t.Fatalf("shutdown response = %#v, %v", shutdown, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
}

func TestWorkerReloadResultsAndHealthFailure(t *testing.T) {
	oldPath := writeSignatureDatabase(t, "old-pattern\n")
	newPath := writeSignatureDatabase(t, "new-pattern\n")
	input := &bytes.Buffer{}
	requests := ipc.NewEncoder(input)
	for _, message := range []protocol.Message{
		{Version: protocol.Version, Type: protocol.TypeHealthCheck},
		{Version: protocol.Version, Type: protocol.TypeReload, SignaturePath: newPath},
		{Version: protocol.Version, Type: protocol.TypeReload, SignaturePath: filepath.Join(t.TempDir(), "missing.txt")},
		{Version: protocol.Version, Type: protocol.TypeShutdown},
	} {
		if err := requests.Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := worker.Run(context.Background(), worker.Config{ScannerID: "scanner-a", WorkerVersion: "1", SignaturePath: oldPath, FailHealth: true}, input, &output); err != nil {
		t.Fatal(err)
	}
	decoder := ipc.NewDecoder(&output)
	health, err := decoder.Decode()
	if err != nil || health.Status != "error" || health.Error == "" {
		t.Fatalf("failed health response = %#v, %v", health, err)
	}
	success, err := decoder.Decode()
	if err != nil || !success.Success || success.SignatureCount != 1 {
		t.Fatalf("successful reload = %#v, %v", success, err)
	}
	failure, err := decoder.Decode()
	if err != nil || failure.Success || !strings.Contains(failure.Error, "stat signature database") {
		t.Fatalf("failed reload = %#v, %v", failure, err)
	}
}

func TestWorkerCrashFaultInjection(t *testing.T) {
	signaturesPath := writeSignatureDatabase(t, "pattern\n")
	input := &bytes.Buffer{}
	if err := ipc.NewEncoder(input).Encode(protocol.Message{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 1, FilePath: "file"}); err != nil {
		t.Fatal(err)
	}
	crashed := false
	err := worker.Run(context.Background(), worker.Config{ScannerID: "scanner-a", WorkerVersion: "1", SignaturePath: signaturesPath, CrashOnScan: true, Crash: func() { crashed = true }}, input, &bytes.Buffer{})
	if !crashed || err == nil || !strings.Contains(err.Error(), "crash function returned") {
		t.Fatalf("crash result = %t, %v", crashed, err)
	}
}

func workerPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		reader.Close()
		writer.Close()
	})
	return reader, writer
}
