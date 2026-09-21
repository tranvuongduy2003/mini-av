package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"miniav/pkg/ipc"
	"miniav/pkg/protocol"
	"miniav/pkg/signatures"
)

type Config struct {
	ScannerID     string
	WorkerVersion string
	SignaturePath string
	Delay         time.Duration
	CrashOnScan   bool
	FailHealth    bool
	Crash         func()
	Logger        *slog.Logger
}

func Run(ctx context.Context, config Config, input io.Reader, output io.Writer) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if input == nil || output == nil {
		return errors.New("worker input and output must not be nil")
	}
	decoder := ipc.NewDecoder(input)
	encoder := ipc.NewEncoder(output)
	return RunSession(ctx, config, decoder.Decode, encoder.Encode)
}

func RunSession(ctx context.Context, config Config, receive func() (protocol.Message, error), send func(protocol.Message) error) error {
	if err := validateConfig(config); err != nil {
		return err
	}
	if receive == nil || send == nil {
		return errors.New("worker receive and send functions must not be nil")
	}
	store, err := signatures.NewStore(config.SignaturePath)
	if err != nil {
		return err
	}
	if config.Crash == nil {
		config.Crash = func() { os.Exit(1) }
	}
	if config.Logger == nil {
		config.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	var writes sync.Mutex
	var scans sync.WaitGroup
	write := func(message protocol.Message) error {
		writes.Lock()
		defer writes.Unlock()
		return send(message)
	}

	for {
		if err := ctx.Err(); err != nil {
			scans.Wait()
			return err
		}
		message, err := receive()
		if err != nil {
			scans.Wait()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		switch message.Type {
		case protocol.TypeHello:
			err = write(protocol.Message{
				Version:       protocol.Version,
				Type:          protocol.TypeHelloAck,
				ScannerID:     config.ScannerID,
				WorkerVersion: config.WorkerVersion,
				Capabilities:  []string{"file_scan", "reload"},
			})
		case protocol.TypeHealthCheck:
			status := "ok"
			errorMessage := ""
			if config.FailHealth {
				status = "error"
				errorMessage = "health check failed by fault injection"
			}
			err = write(protocol.Message{Version: protocol.Version, Type: protocol.TypeHealthResult, Status: status, Error: errorMessage})
		case protocol.TypeScan:
			if config.CrashOnScan {
				config.Crash()
				return errors.New("worker crash function returned")
			}
			scans.Add(1)
			go func(message protocol.Message) {
				defer scans.Done()
				if scanErr := handleScan(config.Delay, store, message, write); scanErr != nil {
					config.Logger.Error("send scan result", "error", scanErr)
				}
			}(message)
		case protocol.TypeReload:
			count, reloadErr := store.Reload(message.SignaturePath)
			response := protocol.Message{Version: protocol.Version, Type: protocol.TypeReloadResult, Success: reloadErr == nil, SignatureCount: count}
			if reloadErr != nil {
				response.Error = reloadErr.Error()
			}
			err = write(response)
		case protocol.TypeShutdown:
			scans.Wait()
			return write(protocol.Message{Version: protocol.Version, Type: protocol.TypeShutdownAck})
		default:
			scans.Wait()
			return fmt.Errorf("worker received unexpected message type %q", message.Type)
		}
		if err != nil {
			scans.Wait()
			return err
		}
	}
}

func handleScan(delay time.Duration, store *signatures.Store, message protocol.Message, write func(protocol.Message) error) error {
	started := time.Now()
	if delay > 0 {
		time.Sleep(delay)
	}
	response := protocol.Message{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: message.RequestID}
	file, err := os.Open(message.FilePath)
	if err == nil {
		var info os.FileInfo
		info, err = file.Stat()
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("scan path %q is not a regular file", message.FilePath)
		}
	}
	if err == nil {
		var found bool
		response.Signature, found, err = store.Match(file)
		if found {
			response.Verdict = protocol.VerdictMalware
		} else {
			response.Verdict = protocol.VerdictClean
		}
	}
	if file != nil {
		if closeErr := file.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
	}
	if err != nil {
		response.Verdict = protocol.VerdictError
		response.Signature = ""
		response.Error = err.Error()
	}
	response.DurationMs = time.Since(started).Milliseconds()
	return write(response)
}

func validateConfig(config Config) error {
	if strings.TrimSpace(config.ScannerID) == "" {
		return errors.New("scanner ID must not be empty")
	}
	if strings.TrimSpace(config.WorkerVersion) == "" {
		return errors.New("worker version must not be empty")
	}
	if strings.TrimSpace(config.SignaturePath) == "" {
		return errors.New("signature path must not be empty")
	}
	if config.Delay < 0 {
		return errors.New("worker delay must not be negative")
	}
	return nil
}
