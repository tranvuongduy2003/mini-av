package tests

import (
	"bytes"
	"context"
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
	binaryName := "miniav"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	miniavBinary = filepath.Join(temporaryDirectory, binaryName)
	build := exec.Command("go", "build", "-race", "-o", miniavBinary, "./cmd/miniav")
	build.Dir = repositoryRoot
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build miniav: %v\n%s", err, output)
		os.Exit(1)
	}

	exitCode := m.Run()
	if err := os.RemoveAll(temporaryDirectory); err != nil {
		fmt.Fprintln(os.Stderr, err)
		if exitCode == 0 {
			exitCode = 1
		}
	}
	os.Exit(exitCode)
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
		{name: "missing manifest", args: []string{"update"}, want: "update requires --manifest <path>"},
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
	if status.exitCode != 0 || status.stdout != "core: running\nworkers: 0\n" || status.stderr != "" {
		t.Fatalf("status result = %#v", status)
	}

	commands := []struct {
		name string
		args []string
	}{
		{name: "scan", args: []string{"scan", "sample.txt"}},
		{name: "reload", args: []string{"reload", "scanner-a", "signatures.txt"}},
		{name: "update", args: []string{"update", "--manifest", "release.json"}},
	}
	for _, command := range commands {
		t.Run(command.name, func(t *testing.T) {
			result := runMiniav(t, 2*time.Second, command.args...)
			if result.exitCode != 1 {
				t.Fatalf("exit code = %d, want 1; stderr = %q", result.exitCode, result.stderr)
			}
			if !strings.Contains(result.stderr, command.name+" is not available until its runtime requirement is implemented") {
				t.Fatalf("stderr = %q", result.stderr)
			}
		})
	}

	shutdown := runMiniav(t, 2*time.Second, "shutdown")
	if shutdown.exitCode != 0 || shutdown.stdout != "core: stopped\n" || shutdown.stderr != "" {
		t.Fatalf("shutdown result = %#v", shutdown)
	}
	core.wait(t)
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
}

func startCore(t *testing.T) *coreProcess {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "miniav.toml")
	if err := os.WriteFile(configPath, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(miniavBinary, "serve", "--config", configPath)
	stderr := &bytes.Buffer{}
	command.Stderr = stderr
	core := &coreProcess{command: command, done: make(chan error, 1), stderr: stderr}
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
