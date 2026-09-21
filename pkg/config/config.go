package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"miniav/pkg/workertransport"
)

type Config struct {
	StartupTimeoutMs int    `json:"startupTimeoutMs"`
	ScanTimeoutMs    int    `json:"scanTimeoutMs"`
	StdioWorker      Worker `json:"stdioWorker"`
	SocketWorker     Worker `json:"socketWorker"`
	GRPCWorker       Worker `json:"grpcWorker"`
	BaseDir          string `json:"-"`
}

type Worker struct {
	ScannerID     string `json:"scannerId"`
	WorkerVersion string `json:"workerVersion"`
	Transport     string `json:"-"`
	Executable    string `json:"executable"`
	SignaturePath string `json:"signaturePath"`
	DelayMs       int    `json:"delayMs,omitempty"`
	CrashOnScan   bool   `json:"crashOnScan,omitempty"`
	FailHealth    bool   `json:"failHealth,omitempty"`
}

func Load(path string) (Config, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return Config{}, fmt.Errorf("resolve config %q: %w", path, err)
	}
	info, err := os.Stat(absolutePath)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("config %q is not a regular file", path)
	}
	data, err := os.ReadFile(absolutePath)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result Config
	if err := decoder.Decode(&result); err != nil {
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %q: multiple JSON values", path)
		}
		return Config{}, fmt.Errorf("decode config %q: %w", path, err)
	}
	result.BaseDir = filepath.Dir(absolutePath)
	if err := result.validate(); err != nil {
		return Config{}, err
	}
	return result, nil
}

func (config Config) StartupTimeout() time.Duration {
	return time.Duration(config.StartupTimeoutMs) * time.Millisecond
}

func (config Config) ScanTimeout() time.Duration {
	return time.Duration(config.ScanTimeoutMs) * time.Millisecond
}

func (config Config) Workers() []Worker {
	return []Worker{config.StdioWorker, config.SocketWorker, config.GRPCWorker}
}

func (config *Config) validate() error {
	if config.StartupTimeoutMs <= 0 {
		return errors.New("startupTimeoutMs must be greater than zero")
	}
	if config.ScanTimeoutMs <= 0 {
		return errors.New("scanTimeoutMs must be greater than zero")
	}
	workers := []struct {
		name      string
		transport string
		worker    *Worker
	}{
		{name: "stdioWorker", transport: workertransport.Stdio, worker: &config.StdioWorker},
		{name: "socketWorker", transport: workertransport.Socket, worker: &config.SocketWorker},
		{name: "grpcWorker", transport: workertransport.GRPC, worker: &config.GRPCWorker},
	}
	seen := make(map[string]struct{}, len(workers))
	for _, configured := range workers {
		worker := configured.worker
		worker.Transport = configured.transport
		if strings.TrimSpace(worker.ScannerID) == "" {
			return fmt.Errorf("%s.scannerId must not be empty", configured.name)
		}
		if _, exists := seen[worker.ScannerID]; exists {
			return fmt.Errorf("scannerId %q is duplicated", worker.ScannerID)
		}
		seen[worker.ScannerID] = struct{}{}
		if strings.TrimSpace(worker.WorkerVersion) == "" {
			return fmt.Errorf("%s.workerVersion must not be empty", configured.name)
		}
		if worker.DelayMs < 0 {
			return fmt.Errorf("%s.delayMs must not be negative", configured.name)
		}
		var err error
		worker.Executable, err = regularPath(config.BaseDir, worker.Executable, "executable")
		if err != nil {
			return fmt.Errorf("%s: %w", configured.name, err)
		}
		worker.SignaturePath, err = regularPath(config.BaseDir, worker.SignaturePath, "signaturePath")
		if err != nil {
			return fmt.Errorf("%s: %w", configured.name, err)
		}
	}
	return nil
}

func regularPath(base string, path string, name string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s must not be empty", name)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(base, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", name, path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat %s %q: %w", name, absolute, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s %q is not a regular file", name, absolute)
	}
	return absolute, nil
}
