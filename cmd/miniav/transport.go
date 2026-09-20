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
	"strings"
	"sync"
	"time"
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
	if err := validateConfigPath(configPath); err != nil {
		return err
	}
	if err := validateLoopbackAddress(address); err != nil {
		return err
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", address, err)
	}
	return serveListener(ctx, listener)
}

func validateConfigPath(configPath string) error {
	info, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", configPath, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("config %q is not a regular file", configPath)
	}
	return nil
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

func serveListener(ctx context.Context, listener net.Listener) error {
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
	defer connections.Wait()
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
			handleConnection(connection, stopListening)
		}()
	}
}

func handleConnection(connection net.Conn, stopListening func()) {
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

	response := dispatchCommand(request)
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

func dispatchCommand(request commandRequest) commandResponse {
	if err := validateCommandRequest(request); err != nil {
		return commandResponse{Error: err.Error()}
	}

	switch request.Command {
	case "status":
		return commandResponse{Success: true, Output: "core: running\nworkers: 0"}
	case "shutdown":
		return commandResponse{Success: true, Output: "core: stopped", Shutdown: true}
	case "scan", "reload", "update":
		return commandResponse{Error: fmt.Sprintf("%s is not available until its runtime requirement is implemented", request.Command)}
	default:
		return commandResponse{Error: fmt.Sprintf("unknown command %q", request.Command)}
	}
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
