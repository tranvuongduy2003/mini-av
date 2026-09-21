package tests

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const coreAddress = "127.0.0.1:7331"

var miniavBinary string
var scannerABinary string
var scannerBBinary string
var scannerCBinary string

func TestMain(m *testing.M) {
	if os.Getenv(processHelperEnvironment) != "" || os.Getenv(coordinatorHelperEnvironment) != "" {
		os.Exit(m.Run())
	}

	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "locate test source")
		os.Exit(1)
	}
	repositoryRoot := filepath.Dir(filepath.Dir(sourceFile))
	temporaryDirectory, err := os.MkdirTemp("", "miniav-tests-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, target := range []struct {
		name string
		path string
		set  func(string)
	}{
		{name: "miniav", path: "./cmd/miniav", set: func(path string) { miniavBinary = path }},
		{name: "scanner-a", path: "./cmd/scanner-a", set: func(path string) { scannerABinary = path }},
		{name: "scanner-b", path: "./cmd/scanner-b", set: func(path string) { scannerBBinary = path }},
		{name: "scanner-c", path: "./cmd/scanner-c", set: func(path string) { scannerCBinary = path }},
	} {
		binaryName := target.name
		if runtime.GOOS == "windows" {
			binaryName += ".exe"
		}
		binaryPath := filepath.Join(temporaryDirectory, binaryName)
		build := exec.Command("go", "build", "-race", "-o", binaryPath, target.path)
		build.Dir = repositoryRoot
		if output, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", target.name, err, output)
			os.Exit(1)
		}
		target.set(binaryPath)
	}

	exitCode := m.Run()
	if err := removeTestDirectory(temporaryDirectory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
}

func removeTestDirectory(path string) error {
	deadline := time.Now().Add(3 * time.Second)
	var err error
	for {
		if err = os.RemoveAll(path); err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestCLIRejectsInvalidCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "missing command", want: "a command is required"},
		{name: "unknown command", args: []string{"help"}, want: `unknown command "help"`},
		{name: "missing config", args: []string{"serve"}, want: "serve requires --config <path>"},
		{name: "serve positional", args: []string{"serve", "--config", "config.toml", "extra"}, want: "serve does not accept positional arguments"},
		{name: "scan count", args: []string{"scan"}, want: "scan requires exactly one file path"},
		{name: "scan empty", args: []string{"scan", " "}, want: "scan requires a non-empty file path"},
		{name: "status arguments", args: []string{"status", "extra"}, want: "status does not accept arguments"},
		{name: "reload count", args: []string{"reload", "scanner-a"}, want: "reload requires a scanner ID and signature path"},
		{name: "reload empty", args: []string{"reload", " ", "signatures.txt"}, want: "reload requires a non-empty scanner ID and signature path"},
		{name: "missing manifest", args: []string{"update"}, want: "update requires --manifest <source>"},
		{name: "shutdown arguments", args: []string{"shutdown", "extra"}, want: "shutdown does not accept arguments"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runMiniav(t, 2*time.Second, test.args...)
			if result.exitCode != 2 {
				t.Fatalf("exit code = %d, want 2; stderr = %q", result.exitCode, result.stderr)
			}
			if !strings.Contains(result.stderr, test.want) {
				t.Fatalf("stderr = %q, want it to contain %q", result.stderr, test.want)
			}
			if !strings.Contains(result.stderr, "miniav serve --config <path>") {
				t.Fatalf("stderr = %q, want usage", result.stderr)
			}
		})
	}
}

func TestServeStatusCommandsAndShutdown(t *testing.T) {
	core := startCore(t)

	status := runMiniav(t, 2*time.Second, "status")
	if status.exitCode != 0 || !strings.Contains(status.stdout, "core: running\nworkers: 3\n") || !strings.Contains(status.stdout, "scanner-a@1.0.0") || !strings.Contains(status.stdout, "state=ACTIVE") || status.stderr != "" {
		t.Fatalf("status result = %#v", status)
	}

	shutdown := runMiniav(t, 2*time.Second, "shutdown")
	if shutdown.exitCode != 0 || shutdown.stdout != "core: stopped\n" || shutdown.stderr != "" {
		t.Fatalf("shutdown result = %#v", shutdown)
	}
	core.wait(t)
}

func TestFixedWorkerTransportTopology(t *testing.T) {
	startCore(t)
	status := runMiniav(t, 2*time.Second, "status")
	wanted := map[string]string{"scanner-a@1.0.0": "stdio", "scanner-b@1.0.0": "socket", "scanner-c@1.0.0": "grpc"}
	for worker, transport := range wanted {
		found := false
		for _, line := range strings.Split(status.stdout, "\n") {
			if strings.HasPrefix(line, worker+" ") && strings.Contains(line, "transport="+transport+" state=ACTIVE") {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("status = %q, want active %s over %s", status.stdout, worker, transport)
		}
	}
}

func TestScanAggregationAndReloadEndToEnd(t *testing.T) {
	core := startCore(t)
	cleanPath := filepath.Join(core.base, "clean.txt")
	writeTestFile(t, cleanPath, []byte("nothing suspicious"), 0o600)
	clean := runMiniav(t, 2*time.Second, "scan", cleanPath)
	if clean.exitCode != 0 || !strings.Contains(clean.stdout, "verdict: CLEAN") || !strings.Contains(clean.stdout, "scanner-a@1.0.0: CLEAN") || !strings.Contains(clean.stdout, "scanner-b@1.0.0: CLEAN") || !strings.Contains(clean.stdout, "scanner-c@1.0.0: CLEAN") {
		t.Fatalf("clean scan = %#v", clean)
	}

	malwarePath := filepath.Join(core.base, "malware.txt")
	writeTestFile(t, malwarePath, []byte("contains pattern-a"), 0o600)
	malware := runMiniav(t, 2*time.Second, "scan", malwarePath)
	if malware.exitCode != 0 || !strings.Contains(malware.stdout, "verdict: MALWARE") || !strings.Contains(malware.stdout, "signature=pattern-a") {
		t.Fatalf("malware scan = %#v", malware)
	}

	reloadedSignatures := filepath.Join(core.base, "reloaded.txt")
	writeTestFile(t, reloadedSignatures, []byte("new-pattern\n"), 0o600)
	reload := runMiniav(t, 2*time.Second, "reload", "scanner-a", reloadedSignatures)
	if reload.exitCode != 0 || !strings.Contains(reload.stdout, "reloaded scanner-a@1.0.0 signatures=1") {
		t.Fatalf("reload = %#v", reload)
	}
	writeTestFile(t, malwarePath, []byte("contains new-pattern"), 0o600)
	afterReload := runMiniav(t, 2*time.Second, "scan", malwarePath)
	if afterReload.exitCode != 0 || !strings.Contains(afterReload.stdout, "verdict: MALWARE") || !strings.Contains(afterReload.stdout, "signature=new-pattern") {
		t.Fatalf("scan after reload = %#v", afterReload)
	}
}

func TestScanTimeoutAndWorkerCrashAreIsolated(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		core := startCoreConfigured(t, 50, func(workers map[string]map[string]any) {
			workers["socketWorker"]["delayMs"] = 250
		})
		path := filepath.Join(core.base, "clean.txt")
		writeTestFile(t, path, []byte("clean"), 0o600)
		result := runMiniav(t, 2*time.Second, "scan", path)
		if result.exitCode != 0 || !strings.Contains(result.stdout, "verdict: INCONCLUSIVE") || !strings.Contains(result.stdout, "scanner-b@1.0.0: TIMEOUT") {
			t.Fatalf("timeout scan = %#v", result)
		}
		status := runMiniav(t, 2*time.Second, "status")
		if status.exitCode != 0 || !strings.Contains(status.stdout, "scanner-b@1.0.0") {
			t.Fatalf("status after timeout = %#v", status)
		}
	})

	t.Run("crash", func(t *testing.T) {
		core := startCoreConfigured(t, 500, func(workers map[string]map[string]any) {
			workers["grpcWorker"]["crashOnScan"] = true
		})
		path := filepath.Join(core.base, "clean.txt")
		writeTestFile(t, path, []byte("clean"), 0o600)
		result := runMiniav(t, 2*time.Second, "scan", path)
		if result.exitCode != 0 || !strings.Contains(result.stdout, "verdict: INCONCLUSIVE") || !strings.Contains(result.stdout, "scanner-c@1.0.0: UNAVAILABLE") {
			t.Fatalf("crash scan = %#v", result)
		}
		status := runMiniav(t, 2*time.Second, "status")
		if status.exitCode != 0 || !strings.Contains(status.stdout, "scanner-c@1.0.0") || !strings.Contains(status.stdout, "state=FAILED") || !strings.Contains(status.stdout, "scanner-a@1.0.0") {
			t.Fatalf("status after crash = %#v", status)
		}
	})
}

func TestBlueGreenUpdateAndRollbackEndToEnd(t *testing.T) {
	core := startCoreConfigured(t, 1000, func(workers map[string]map[string]any) {
		workers["stdioWorker"]["delayMs"] = 300
	})
	manifestV2 := writeReleaseManifest(t, core.base, "2.0.0", scannerABinary)
	scanPath := filepath.Join(core.base, "in-flight.txt")
	writeTestFile(t, scanPath, []byte("clean"), 0o600)
	scanDone := make(chan commandResult, 1)
	go func() { scanDone <- runMiniav(t, 2*time.Second, "scan", scanPath) }()
	time.Sleep(75 * time.Millisecond)
	updated := runMiniav(t, 4*time.Second, "update", "--manifest", manifestV2)
	if updated.exitCode != 0 || !strings.Contains(updated.stdout, "updated scanner-a from 1.0.0 to 2.0.0") {
		t.Fatalf("update = %#v", updated)
	}
	if scan := <-scanDone; scan.exitCode != 0 || !strings.Contains(scan.stdout, "scanner-a@1.0.0: CLEAN") {
		t.Fatalf("in-flight scan = %#v", scan)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		status := runMiniav(t, 2*time.Second, "status")
		if status.exitCode == 0 && strings.Contains(status.stdout, "scanner-a@2.0.0") && strings.Contains(status.stdout, "state=ACTIVE") && strings.Contains(status.stdout, "scanner-a@1.0.0") && strings.Contains(status.stdout, "state=RETIRED") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workers did not complete cutover: %#v", status)
		}
		time.Sleep(25 * time.Millisecond)
	}

	broken := filepath.Join(core.base, "broken-worker")
	writeTestFile(t, broken, []byte("not an executable"), 0o700)
	manifestV3 := writeReleaseManifest(t, core.base, "3.0.0", broken)
	rollback := runMiniav(t, 4*time.Second, "update", "--manifest", manifestV3)
	if rollback.exitCode != 1 || !strings.Contains(rollback.stderr, "update rolled back") {
		t.Fatalf("rollback = %#v", rollback)
	}
	status := runMiniav(t, 2*time.Second, "status")
	if status.exitCode != 0 || !strings.Contains(status.stdout, "scanner-a@2.0.0") || !strings.Contains(status.stdout, "state=ACTIVE") {
		t.Fatalf("status after rollback = %#v", status)
	}
}

func TestServeRejectsInvalidConfigPaths(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.toml")
	result := runMiniav(t, 2*time.Second, "serve", "--config", missing)
	if result.exitCode != 1 || !strings.Contains(result.stderr, "read config") {
		t.Fatalf("missing config result = %#v", result)
	}

	result = runMiniav(t, 2*time.Second, "serve", "--config", t.TempDir())
	if result.exitCode != 1 || !strings.Contains(result.stderr, "is not a regular file") {
		t.Fatalf("directory config result = %#v", result)
	}
}

func TestControlProtocolRejectsInvalidRequests(t *testing.T) {
	core := startCore(t)
	defer core.stop(t)

	tests := []struct {
		name    string
		request map[string]any
		want    string
	}{
		{name: "unknown command", request: map[string]any{"command": "other"}, want: "unknown command"},
		{name: "wrong argument count", request: map[string]any{"command": "reload", "args": []string{"scanner-a"}}, want: "received 1 arguments; want 2"},
		{name: "empty argument", request: map[string]any{"command": "scan", "args": []string{" "}}, want: "arguments must not be empty"},
		{name: "unknown field", request: map[string]any{"command": "status", "unexpected": true}, want: "unknown field"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := sendControlRequest(t, test.request)
			if response.Success || !strings.Contains(response.Error, test.want) {
				t.Fatalf("response = %#v, want error containing %q", response, test.want)
			}
		})
	}
}

func TestClientRequestTimesOut(t *testing.T) {
	listener, err := net.Listen("tcp", coreAddress)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()

	result := runMiniav(t, 7*time.Second, "status")
	if result.exitCode != 1 || !strings.Contains(result.stderr, "read response from core") {
		t.Fatalf("timeout result = %#v", result)
	}

	select {
	case connection := <-accepted:
		connection.Close()
	case <-time.After(time.Second):
		t.Fatal("fake core did not accept the client connection")
	}
}

type commandResult struct {
	exitCode int
	stdout   string
	stderr   string
}

func runMiniav(t *testing.T, timeout time.Duration, args ...string) commandResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, miniavBinary, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatalf("miniav %v exceeded %s", args, timeout)
	}
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatalf("run miniav %v: %v", args, err)
		}
		exitCode = exitError.ExitCode()
	}
	return commandResult{exitCode: exitCode, stdout: stdout.String(), stderr: stderr.String()}
}

type coreProcess struct {
	command *exec.Cmd
	done    chan error
	stderr  *bytes.Buffer
	waited  bool
	base    string
}

func startCore(t *testing.T) *coreProcess {
	return startCoreConfigured(t, 500, nil)
}

func startCoreConfigured(t *testing.T, scanTimeoutMs int, modify func(map[string]map[string]any)) *coreProcess {
	t.Helper()
	base := t.TempDir()
	signatureA := filepath.Join(base, "scanner-a.txt")
	signatureB := filepath.Join(base, "scanner-b.txt")
	signatureC := filepath.Join(base, "scanner-c.txt")
	if err := os.WriteFile(signatureA, []byte("pattern-a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signatureB, []byte("pattern-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signatureC, []byte("pattern-c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(base, "miniav.json")
	workers := map[string]map[string]any{
		"stdioWorker":  {"scannerId": "scanner-a", "workerVersion": "1.0.0", "executable": scannerABinary, "signaturePath": signatureA},
		"socketWorker": {"scannerId": "scanner-b", "workerVersion": "1.0.0", "executable": scannerBBinary, "signaturePath": signatureB},
		"grpcWorker":   {"scannerId": "scanner-c", "workerVersion": "1.0.0", "executable": scannerCBinary, "signaturePath": signatureC},
	}
	if modify != nil {
		modify(workers)
	}
	configData, err := json.Marshal(map[string]any{
		"startupTimeoutMs": 2000,
		"scanTimeoutMs":    scanTimeoutMs,
		"stdioWorker":      workers["stdioWorker"],
		"socketWorker":     workers["socketWorker"],
		"grpcWorker":       workers["grpcWorker"],
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(miniavBinary, "serve", "--config", configPath)
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	core := &coreProcess{command: command, done: make(chan error, 1), stderr: stderr, base: base}
	if err := command.Start(); err != nil {
		t.Fatalf("start core: %v", err)
	}
	go func() {
		core.done <- command.Wait()
	}()
	t.Cleanup(func() {
		core.stop(t)
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		connection, err := net.DialTimeout("tcp", coreAddress, 50*time.Millisecond)
		if err == nil {
			connection.Close()
			return core
		}
		select {
		case err := <-core.done:
			core.waited = true
			t.Fatalf("core exited during startup: %v; stderr = %q", err, stderr.String())
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	command.Process.Kill()
	<-core.done
	core.waited = true
	t.Fatalf("core did not start; stderr = %q", stderr.String())
	return nil
}

func writeReleaseManifest(t *testing.T, base string, version string, source string) string {
	t.Helper()
	content, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(base, "releases", "scanner-a", version, executableName("scanner-a"))
	writeTestFile(t, artifact, content, 0o700)
	digest := sha256.Sum256(content)
	manifestPath := filepath.Join(base, "releases", "scanner-a", version, "manifest.json")
	manifest := map[string]any{
		"scannerId": "scanner-a", "workerVersion": version, "signatureVersion": "1",
		"artifactPath": artifact, "sha256": hex.EncodeToString(digest[:]),
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, manifestPath, data, 0o600)
	return manifestPath
}

func (core *coreProcess) wait(t *testing.T) {
	t.Helper()
	if core.waited {
		return
	}
	select {
	case err := <-core.done:
		core.waited = true
		if err != nil {
			t.Fatalf("core exited with error: %v; stderr = %q", err, core.stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("core did not stop; stderr = %q", core.stderr.String())
	}
}

func (core *coreProcess) stop(t *testing.T) {
	t.Helper()
	if core == nil || core.waited {
		return
	}
	shutdown := runMiniav(t, 2*time.Second, "shutdown")
	if shutdown.exitCode != 0 {
		core.command.Process.Kill()
	}
	core.wait(t)
}

type controlResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

func sendControlRequest(t *testing.T, request any) controlResponse {
	t.Helper()
	connection, err := net.DialTimeout("tcp", coreAddress, time.Second)
	if err != nil {
		t.Fatalf("connect to core: %v", err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		t.Fatalf("write request: %v", err)
	}
	var response controlResponse
	if err := json.NewDecoder(connection).Decode(&response); err != nil {
		t.Fatalf("read response: %v", err)
	}
	return response
}
