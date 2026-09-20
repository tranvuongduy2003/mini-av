package protocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const Version = 1

const (
	TypeHello        = "HELLO"
	TypeHealthCheck  = "HEALTH_CHECK"
	TypeScan         = "SCAN"
	TypeReload       = "RELOAD"
	TypeShutdown     = "SHUTDOWN"
	TypeHelloAck     = "HELLO_ACK"
	TypeHealthResult = "HEALTH_RESULT"
	TypeScanResult   = "SCAN_RESULT"
	TypeReloadResult = "RELOAD_RESULT"
	TypeShutdownAck  = "SHUTDOWN_ACK"
)

type Verdict string

const (
	VerdictClean       Verdict = "CLEAN"
	VerdictMalware     Verdict = "MALWARE"
	VerdictError       Verdict = "ERROR"
	VerdictTimeout     Verdict = "TIMEOUT"
	VerdictUnavailable Verdict = "UNAVAILABLE"
)

type Message struct {
	Version        int      `json:"version"`
	Type           string   `json:"type"`
	RequestID      uint64   `json:"request_id,omitempty"`
	ScannerID      string   `json:"scanner_id,omitempty"`
	WorkerVersion  string   `json:"worker_version,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	FilePath       string   `json:"file_path,omitempty"`
	SignaturePath  string   `json:"signature_path,omitempty"`
	Status         string   `json:"status,omitempty"`
	Verdict        Verdict  `json:"verdict,omitempty"`
	Signature      string   `json:"signature,omitempty"`
	DurationMs     int64    `json:"duration_ms,omitempty"`
	Success        bool     `json:"success,omitempty"`
	SignatureCount int      `json:"signature_count,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type wireMessage struct {
	Version        int      `json:"version"`
	Type           string   `json:"type"`
	RequestID      *uint64  `json:"request_id,omitempty"`
	ScannerID      string   `json:"scanner_id,omitempty"`
	WorkerVersion  string   `json:"worker_version,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	FilePath       string   `json:"file_path,omitempty"`
	SignaturePath  string   `json:"signature_path,omitempty"`
	Status         string   `json:"status,omitempty"`
	Verdict        Verdict  `json:"verdict,omitempty"`
	Signature      string   `json:"signature,omitempty"`
	DurationMs     *int64   `json:"duration_ms,omitempty"`
	Success        *bool    `json:"success,omitempty"`
	SignatureCount *int     `json:"signature_count,omitempty"`
	Error          string   `json:"error,omitempty"`
}

func Validate(message Message) error {
	if message.Version != Version {
		return fmt.Errorf("protocol version = %d, want %d", message.Version, Version)
	}

	switch message.Type {
	case TypeHello, TypeHealthCheck, TypeShutdown, TypeShutdownAck:
	case TypeScan:
		if message.RequestID == 0 {
			return errors.New("SCAN requires a non-zero request_id")
		}
		if strings.TrimSpace(message.FilePath) == "" {
			return errors.New("SCAN requires a non-empty file_path")
		}
	case TypeReload:
		if strings.TrimSpace(message.SignaturePath) == "" {
			return errors.New("RELOAD requires a non-empty signature_path")
		}
	case TypeHelloAck:
		if strings.TrimSpace(message.ScannerID) == "" {
			return errors.New("HELLO_ACK requires a non-empty scanner_id")
		}
		if strings.TrimSpace(message.WorkerVersion) == "" {
			return errors.New("HELLO_ACK requires a non-empty worker_version")
		}
		if message.Capabilities == nil {
			return errors.New("HELLO_ACK requires capabilities")
		}
		seen := make(map[string]struct{}, len(message.Capabilities))
		for _, capability := range message.Capabilities {
			if strings.TrimSpace(capability) == "" {
				return errors.New("HELLO_ACK capabilities must not contain empty values")
			}
			if _, exists := seen[capability]; exists {
				return fmt.Errorf("HELLO_ACK capability %q is duplicated", capability)
			}
			seen[capability] = struct{}{}
		}
	case TypeHealthResult:
		if message.Status != "ok" && message.Status != "error" {
			return errors.New("HEALTH_RESULT status must be ok or error")
		}
	case TypeScanResult:
		if message.RequestID == 0 {
			return errors.New("SCAN_RESULT requires a non-zero request_id")
		}
		if !validVerdict(message.Verdict) {
			return fmt.Errorf("SCAN_RESULT verdict %q must be CLEAN, MALWARE, ERROR, TIMEOUT, or UNAVAILABLE", message.Verdict)
		}
		if message.DurationMs < 0 {
			return errors.New("SCAN_RESULT duration_ms must not be negative")
		}
	case TypeReloadResult:
		if message.SignatureCount < 0 {
			return errors.New("RELOAD_RESULT signature_count must not be negative")
		}
		if !message.Success && strings.TrimSpace(message.Error) == "" {
			return errors.New("failed RELOAD_RESULT requires a non-empty error")
		}
	default:
		if strings.TrimSpace(message.Type) == "" {
			return errors.New("message type is required")
		}
		return fmt.Errorf("unknown message type %q", message.Type)
	}

	return validateAllowedFields(message)
}

func validVerdict(verdict Verdict) bool {
	switch verdict {
	case VerdictClean, VerdictMalware, VerdictError, VerdictTimeout, VerdictUnavailable:
		return true
	default:
		return false
	}
}

func Marshal(message Message) ([]byte, error) {
	if err := Validate(message); err != nil {
		return nil, err
	}
	wire := toWire(message)
	data, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal protocol message: %w", err)
	}
	return data, nil
}

func Unmarshal(data []byte) (Message, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire wireMessage
	if err := decoder.Decode(&wire); err != nil {
		return Message{}, fmt.Errorf("decode protocol message: %w", err)
	}
	if err := requireEnd(decoder); err != nil {
		return Message{}, err
	}
	if err := validateWireFieldPlacement(wire); err != nil {
		return Message{}, err
	}
	if err := validateRequiredWireFields(wire); err != nil {
		return Message{}, err
	}
	message := fromWire(wire)
	if err := Validate(message); err != nil {
		return Message{}, err
	}
	return message, nil
}

func requireEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("decode protocol message: %w", err)
	}
	return errors.New("decode protocol message: multiple JSON values in one frame")
}

func validateRequiredWireFields(message wireMessage) error {
	switch message.Type {
	case TypeScan, TypeScanResult:
		if message.RequestID == nil {
			return fmt.Errorf("%s requires request_id", message.Type)
		}
	}
	switch message.Type {
	case TypeScanResult:
		if message.DurationMs == nil {
			return errors.New("SCAN_RESULT requires duration_ms")
		}
	case TypeReloadResult:
		if message.Success == nil {
			return errors.New("RELOAD_RESULT requires success")
		}
		if *message.Success && message.SignatureCount == nil {
			return errors.New("RELOAD_RESULT requires signature_count")
		}
	}
	return nil
}

func toWire(message Message) wireMessage {
	wire := wireMessage{
		Version:       message.Version,
		Type:          message.Type,
		ScannerID:     message.ScannerID,
		WorkerVersion: message.WorkerVersion,
		Capabilities:  message.Capabilities,
		FilePath:      message.FilePath,
		SignaturePath: message.SignaturePath,
		Status:        message.Status,
		Verdict:       message.Verdict,
		Signature:     message.Signature,
		Error:         message.Error,
	}
	if message.Type == TypeScan || message.Type == TypeScanResult {
		wire.RequestID = &message.RequestID
	}
	if message.Type == TypeScanResult {
		wire.DurationMs = &message.DurationMs
	}
	if message.Type == TypeReloadResult {
		wire.Success = &message.Success
		if message.Success || message.SignatureCount != 0 {
			wire.SignatureCount = &message.SignatureCount
		}
	}
	return wire
}

func fromWire(wire wireMessage) Message {
	message := Message{
		Version:       wire.Version,
		Type:          wire.Type,
		ScannerID:     wire.ScannerID,
		WorkerVersion: wire.WorkerVersion,
		Capabilities:  wire.Capabilities,
		FilePath:      wire.FilePath,
		SignaturePath: wire.SignaturePath,
		Status:        wire.Status,
		Verdict:       wire.Verdict,
		Signature:     wire.Signature,
		Error:         wire.Error,
	}
	if wire.RequestID != nil {
		message.RequestID = *wire.RequestID
	}
	if wire.DurationMs != nil {
		message.DurationMs = *wire.DurationMs
	}
	if wire.Success != nil {
		message.Success = *wire.Success
	}
	if wire.SignatureCount != nil {
		message.SignatureCount = *wire.SignatureCount
	}
	return message
}

func validateAllowedFields(message Message) error {
	fields := []struct {
		name string
		set  bool
	}{
		{name: "request_id", set: message.RequestID != 0},
		{name: "scanner_id", set: message.ScannerID != ""},
		{name: "worker_version", set: message.WorkerVersion != ""},
		{name: "capabilities", set: message.Capabilities != nil},
		{name: "file_path", set: message.FilePath != ""},
		{name: "signature_path", set: message.SignaturePath != ""},
		{name: "status", set: message.Status != ""},
		{name: "verdict", set: message.Verdict != ""},
		{name: "signature", set: message.Signature != ""},
		{name: "duration_ms", set: message.DurationMs != 0},
		{name: "success", set: message.Success},
		{name: "signature_count", set: message.SignatureCount != 0},
		{name: "error", set: message.Error != ""},
	}
	for _, field := range fields {
		if field.set && !fieldAllowed(message.Type, field.name) {
			return fmt.Errorf("%s does not allow field %s", message.Type, field.name)
		}
	}
	return nil
}

func validateWireFieldPlacement(message wireMessage) error {
	fields := []struct {
		name string
		set  bool
	}{
		{name: "request_id", set: message.RequestID != nil},
		{name: "duration_ms", set: message.DurationMs != nil},
		{name: "success", set: message.Success != nil},
		{name: "signature_count", set: message.SignatureCount != nil},
	}
	for _, field := range fields {
		if field.set && !fieldAllowed(message.Type, field.name) {
			return fmt.Errorf("%s does not allow field %s", message.Type, field.name)
		}
	}
	return nil
}

func fieldAllowed(messageType string, field string) bool {
	switch messageType {
	case TypeScan:
		return field == "request_id" || field == "file_path"
	case TypeReload:
		return field == "signature_path"
	case TypeHelloAck:
		return field == "scanner_id" || field == "worker_version" || field == "capabilities"
	case TypeHealthResult:
		return field == "status" || field == "error"
	case TypeScanResult:
		return field == "request_id" || field == "verdict" || field == "signature" || field == "duration_ms" || field == "error"
	case TypeReloadResult:
		return field == "success" || field == "signature_count" || field == "error"
	default:
		return false
	}
}
