package tests

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"miniav/pkg/protocol"
)

func TestProtocolMessagesRoundTrip(t *testing.T) {
	messages := []protocol.Message{
		{Version: protocol.Version, Type: protocol.TypeHello},
		{Version: protocol.Version, Type: protocol.TypeHealthCheck},
		{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 101, FilePath: `C:\path\file.txt`},
		{Version: protocol.Version, Type: protocol.TypeReload, SignaturePath: `C:\path\signatures.txt`},
		{Version: protocol.Version, Type: protocol.TypeShutdown},
		{Version: protocol.Version, Type: protocol.TypeHelloAck, ScannerID: "scanner-a", WorkerVersion: "1.0.0", Capabilities: []string{"file_scan", "reload"}},
		{Version: protocol.Version, Type: protocol.TypeHealthResult, Status: "ok"},
		{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: 101, Verdict: protocol.VerdictMalware, Signature: "VIRUS_SAMPLE_XYZ", DurationMs: 12},
		{Version: protocol.Version, Type: protocol.TypeReloadResult, Success: true, SignatureCount: 50},
		{Version: protocol.Version, Type: protocol.TypeShutdownAck},
	}

	for _, message := range messages {
		t.Run(message.Type, func(t *testing.T) {
			data, err := protocol.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := protocol.Unmarshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, message) {
				t.Fatalf("decoded = %#v, want %#v", decoded, message)
			}
		})
	}
}

func TestProtocolScanResultVerdictsRoundTrip(t *testing.T) {
	verdicts := []protocol.Verdict{
		protocol.VerdictClean,
		protocol.VerdictMalware,
		protocol.VerdictError,
		protocol.VerdictTimeout,
		protocol.VerdictUnavailable,
	}

	for _, verdict := range verdicts {
		t.Run(string(verdict), func(t *testing.T) {
			message := protocol.Message{
				Version:   protocol.Version,
				Type:      protocol.TypeScanResult,
				RequestID: 1,
				Verdict:   verdict,
			}
			data, err := protocol.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), `"verdict":"`+string(verdict)+`"`) {
				t.Fatalf("message = %s, want verdict %q", data, verdict)
			}
			decoded, err := protocol.Unmarshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded, message) {
				t.Fatalf("decoded = %#v, want %#v", decoded, message)
			}
		})
	}
}

func TestProtocolRejectsInvalidScanResultVerdicts(t *testing.T) {
	verdicts := []protocol.Verdict{"", "UNKNOWN", "clean", " CLEAN "}

	for _, verdict := range verdicts {
		name := string(verdict)
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			message := protocol.Message{
				Version:   protocol.Version,
				Type:      protocol.TypeScanResult,
				RequestID: 1,
				Verdict:   verdict,
			}
			if err := protocol.Validate(message); err == nil || !strings.Contains(err.Error(), "verdict") {
				t.Fatalf("Validate error = %v, want verdict error", err)
			}

			data := `{"version":1,"type":"SCAN_RESULT","request_id":1,"verdict":` + fmt.Sprintf("%q", verdict) + `,"duration_ms":0}`
			if _, err := protocol.Unmarshal([]byte(data)); err == nil || !strings.Contains(err.Error(), "verdict") {
				t.Fatalf("Unmarshal error = %v, want verdict error", err)
			}
		})
	}
}

func TestProtocolReloadFailureKeepsFalseSuccessField(t *testing.T) {
	message := protocol.Message{Version: protocol.Version, Type: protocol.TypeReloadResult, Error: "reload failed"}
	data, err := protocol.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"success":false`) {
		t.Fatalf("message = %s, want explicit false success", data)
	}
	if strings.Contains(string(data), "signature_count") {
		t.Fatalf("message = %s, want signature_count omitted on failure", data)
	}
	decoded, err := protocol.Unmarshal(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, message) {
		t.Fatalf("decoded = %#v, want %#v", decoded, message)
	}
}

func TestProtocolUnmarshalRejectsInvalidMessages(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "wrong version", data: `{"version":2,"type":"HELLO"}`, want: "protocol version"},
		{name: "unknown type", data: `{"version":1,"type":"OTHER"}`, want: "unknown message type"},
		{name: "missing request id", data: `{"version":1,"type":"SCAN","file_path":"sample.txt"}`, want: "requires request_id"},
		{name: "empty path", data: `{"version":1,"type":"SCAN","request_id":1,"file_path":" "}`, want: "non-empty file_path"},
		{name: "missing worker version", data: `{"version":1,"type":"HELLO_ACK","scanner_id":"scanner-a","capabilities":[]}`, want: "worker_version"},
		{name: "invalid health", data: `{"version":1,"type":"HEALTH_RESULT","status":"unknown"}`, want: "status must be"},
		{name: "missing duration", data: `{"version":1,"type":"SCAN_RESULT","request_id":1,"verdict":"CLEAN"}`, want: "requires duration_ms"},
		{name: "missing success", data: `{"version":1,"type":"RELOAD_RESULT","signature_count":0}`, want: "requires success"},
		{name: "failed reload without error", data: `{"version":1,"type":"RELOAD_RESULT","success":false}`, want: "non-empty error"},
		{name: "known field on wrong type", data: `{"version":1,"type":"HELLO","file_path":"sample.txt"}`, want: "does not allow field file_path"},
		{name: "unknown field", data: `{"version":1,"type":"HELLO","extra":true}`, want: "unknown field"},
		{name: "multiple values", data: `{"version":1,"type":"HELLO"} {}`, want: "multiple JSON values"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := protocol.Unmarshal([]byte(test.data))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}
}

func TestProtocolValidateRejectsDuplicateCapabilities(t *testing.T) {
	err := protocol.Validate(protocol.Message{
		Version:       protocol.Version,
		Type:          protocol.TypeHelloAck,
		ScannerID:     "scanner-a",
		WorkerVersion: "1.0.0",
		Capabilities:  []string{"reload", "reload"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("error = %v, want duplicate capability error", err)
	}
}
