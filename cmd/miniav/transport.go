package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"miniav/pkg/config"
	"miniav/pkg/coordinator"
	updatemanager "miniav/pkg/update"
)

const (
	defaultCoreAddress = "127.0.0.1:7331"
	requestTimeout     = 5 * time.Second
	maxFrameSize       = 64 * 1024
)

type commandRequest struct {
	Command string   `json:"command"`
	Args    []string `json:"args,omitempty"`
}

type commandResponse struct {
	Success  bool   `json:"success"`
	Output   string `json:"output,omitempty"`
	Error    string `json:"error,omitempty"`
	Shutdown bool   `json:"shutdown,omitempty"`
}

func sendCommand(ctx context.Context, address string, request commandRequest) (commandResponse, error) {
	requestCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	connection, err := (&net.Dialer{}).DialContext(requestCtx, "tcp", address)
	if err != nil {
		return commandResponse{}, fmt.Errorf("connect to core at %s: %w", address, err)
	}
	defer connection.Close()

	deadline, ok := requestCtx.Deadline()
	if ok {
		if err := connection.SetDeadline(deadline); err != nil {
			return commandResponse{}, fmt.Errorf("set core request deadline: %w", err)
		}
	}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return commandResponse{}, fmt.Errorf("send command to core: %w", err)
	}

	frame, err := readFrame(connection)
	if err != nil {
		return commandResponse{}, fmt.Errorf("read response from core: %w", err)
	}
	var response commandResponse
	if err := json.Unmarshal(frame, &response); err != nil {
		return commandResponse{}, fmt.Errorf("decode response from core: %w", err)
	}
	return response, nil
}

func serve(ctx context.Context, address string, configPath string) error {
	if err := validateLoopbackAddress(address); err != nil {
		return err
	}
	loaded, err := config.Load(configPath)
	if err != nil {
		return err
	}
	runtime, err := coordinator.NewRuntime(ctx, loaded.StartupTimeout(), loaded.ScanTimeout(), os.Stderr)
	if err != nil {
		return err
	}
	for _, worker := range loaded.Workers {
		if err := runtime.StartWorker(ctx, worker); err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), loaded.StartupTimeout())
			runtime.Shutdown(shutdownCtx)
			cancel()
			return fmt.Errorf("start configured worker %q: %w", worker.ScannerID, err)
		}
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), loaded.StartupTimeout())
		runtime.Shutdown(shutdownCtx)
		cancel()
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	return serveListener(ctx, listener, runtime, loaded.BaseDir, loaded.StartupTimeout())
}

func validateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid core address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("core address %q must use a loopback IP", address)
	}
	return nil
}

func serveListener(ctx context.Context, listener net.Listener, runtime *coordinator.Runtime, configDir string, shutdownTimeout time.Duration) error {
	stop := make(chan struct{})
	var stopOnce sync.Once
	stopListening := func() {
		stopOnce.Do(func() {
			close(stop)
			listener.Close()
		})
	}

	go func() {
		select {
		case <-ctx.Done():
			stopListening()
		case <-stop:
		}
	}()

	var connections sync.WaitGroup
	defer func() {
		connections.Wait()
		select {
		case <-runtime.Done():
		default:
			shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			runtime.Shutdown(shutdownCtx)
			cancel()
		}
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			select {
			case <-stop:
				return nil
			default:
				return fmt.Errorf("accept core connection: %w", err)
			}
		}
		connections.Add(1)
		go func() {
			defer connections.Done()
			handleConnection(connection, stopListening, runtime, configDir)
		}()
	}
}

func handleConnection(connection net.Conn, stopListening func(), runtime *coordinator.Runtime, configDir string) {
	defer connection.Close()
	connection.SetDeadline(time.Now().Add(requestTimeout))

	frame, err := readFrame(connection)
	if err != nil {
		writeResponse(connection, commandResponse{Error: fmt.Sprintf("read command: %v", err)})
		return
	}

	var request commandRequest
	decoder := json.NewDecoder(strings.NewReader(string(frame)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeResponse(connection, commandResponse{Error: fmt.Sprintf("decode command: %v", err)})
		return
	}

	requestCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	response := dispatchCommand(requestCtx, request, runtime, configDir)
	if err := writeResponse(connection, response); err != nil {
		return
	}
	if response.Shutdown {
		stopListening()
	}
}

func writeResponse(writer io.Writer, response commandResponse) error {
	return json.NewEncoder(writer).Encode(response)
}

func dispatchCommand(ctx context.Context, request commandRequest, runtime *coordinator.Runtime, configDir string) commandResponse {
	if err := validateCommandRequest(request); err != nil {
		return commandResponse{Error: err.Error()}
	}

	switch request.Command {
	case "status":
		snapshot, err := runtime.Snapshot(ctx)
		if err != nil {
			return commandResponse{Error: err.Error()}
		}
		lines := []string{"core: running", fmt.Sprintf("workers: %d", len(snapshot.Workers))}
		for _, worker := range snapshot.Workers {
			line := fmt.Sprintf("%s pid=%d state=%s", worker.Key, worker.PID, worker.State)
			if worker.FailureReason != "" {
				line += " reason=" + worker.FailureReason
			}
			lines = append(lines, line)
		}
		return commandResponse{Success: true, Output: strings.Join(lines, "\n")}
	case "shutdown":
		if err := runtime.Shutdown(ctx); err != nil {
			return commandResponse{Error: err.Error()}
		}
		return commandResponse{Success: true, Output: "core: stopped", Shutdown: true}
	case "scan":
		result, err := runtime.Scan(ctx, resolveControlPath(configDir, request.Args[0]))
		if err != nil {
			return commandResponse{Error: err.Error()}
		}
		keys := make([]coordinator.WorkerKey, 0, len(result.Results))
		for key := range result.Results {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		lines := []string{fmt.Sprintf("verdict: %s", result.Verdict), fmt.Sprintf("request_id: %d", result.RequestID)}
		for _, key := range keys {
			workerResult := result.Results[key]
			line := fmt.Sprintf("%s: %s", key, workerResult.Verdict)
			if workerResult.Signature != "" {
				line += " signature=" + workerResult.Signature
			}
			lines = append(lines, line)
		}
		return commandResponse{Success: true, Output: strings.Join(lines, "\n")}
	case "reload":
		result, err := runtime.Reload(ctx, request.Args[0], resolveControlPath(configDir, request.Args[1]))
		if err != nil {
			return commandResponse{Error: err.Error()}
		}
		return commandResponse{Success: true, Output: fmt.Sprintf("reloaded %s@%s signatures=%d", result.ScannerID, result.WorkerVersion, result.SignatureCount)}
	case "update":
		prepared, err := updatemanager.Prepare(ctx, configDir, request.Args[0])
		if err != nil {
			return commandResponse{Error: err.Error()}
		}
		active, err := runtime.ActiveWorker(ctx, prepared.Manifest.ScannerID)
		if err != nil {
			return commandResponse{Error: err.Error()}
		}
		candidate := config.Worker{ScannerID: prepared.Manifest.ScannerID, WorkerVersion: prepared.Manifest.WorkerVersion, Executable: prepared.StagedPath, SignaturePath: active.SignaturePath}
		if err := runtime.StartWorker(ctx, candidate); err != nil {
			return commandResponse{Error: fmt.Sprintf("update rolled back: %v", err)}
		}
		return commandResponse{Success: true, Output: fmt.Sprintf("updated %s from %s to %s", candidate.ScannerID, active.WorkerVersion, candidate.WorkerVersion)}
	default:
		return commandResponse{Error: fmt.Sprintf("unknown command %q", request.Command)}
	}
}

func resolveControlPath(base string, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(base, path)
}

func validateCommandRequest(request commandRequest) error {
	wantedArguments := map[string]int{
		"scan":     1,
		"status":   0,
		"reload":   2,
		"update":   1,
		"shutdown": 0,
	}

	wanted, ok := wantedArguments[request.Command]
	if !ok {
		return fmt.Errorf("unknown command %q", request.Command)
	}
	if len(request.Args) != wanted {
		return fmt.Errorf("%s received %d arguments; want %d", request.Command, len(request.Args), wanted)
	}
	for _, argument := range request.Args {
		if strings.TrimSpace(argument) == "" {
			return fmt.Errorf("%s arguments must not be empty", request.Command)
		}
	}
	return nil
}

func readFrame(reader io.Reader) ([]byte, error) {
	buffered := bufio.NewReader(io.LimitReader(reader, maxFrameSize+1))
	frame, err := buffered.ReadBytes('\n')
	if len(frame) > maxFrameSize {
		return nil, errors.New("frame exceeds size limit")
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("frame is not newline terminated")
		}
		return nil, err
	}
	return frame, nil
}
