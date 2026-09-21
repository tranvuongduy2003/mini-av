package tests

import (
	"reflect"
	"testing"

	"miniav/pkg/grpcprotocol"
	"miniav/pkg/protocol"
	"miniav/pkg/workertransport"
)

func TestWorkerTransportRequiresLiteralLoopbackAddress(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "[::1]:0"} {
		if err := workertransport.ValidateLoopbackAddress(address); err != nil {
			t.Fatalf("loopback address %q rejected: %v", address, err)
		}
	}
	for _, address := range []string{"0.0.0.0:0", "localhost:7332", "missing-port"} {
		if err := workertransport.ValidateLoopbackAddress(address); err == nil {
			t.Fatalf("non-loopback address %q accepted", address)
		}
	}
}

func TestGRPCProtobufRoundTrip(t *testing.T) {
	messages := []protocol.Message{
		{Version: protocol.Version, Type: protocol.TypeHello},
		{Version: protocol.Version, Type: protocol.TypeHelloAck, ScannerID: "scanner-c", WorkerVersion: "1", Capabilities: []string{"file_scan", "reload"}},
		{Version: protocol.Version, Type: protocol.TypeScan, RequestID: 1, FilePath: "sample.txt"},
		{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: 1, Verdict: protocol.VerdictClean, DurationMs: 0},
		{Version: protocol.Version, Type: protocol.TypeReloadResult, Success: true, SignatureCount: 0},
	}
	for _, message := range messages {
		payload, err := workertransport.ToProto(message)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := workertransport.FromProto(payload)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(decoded, message) {
			t.Fatalf("round trip = %#v, want %#v", decoded, message)
		}
	}
}

func TestGRPCProtobufRejectsInvalidMessages(t *testing.T) {
	missingDuration := &grpcprotocol.Message{Version: protocol.Version, Type: protocol.TypeScanResult, RequestId: 1, Verdict: grpcprotocol.Verdict_VERDICT_CLEAN}
	if _, err := workertransport.FromProto(missingDuration); err == nil {
		t.Fatal("SCAN_RESULT without duration_ms was accepted")
	}
	unknownVerdict := missingDuration
	unknownVerdict.DurationMs = new(int64)
	unknownVerdict.Verdict = grpcprotocol.Verdict(99)
	if _, err := workertransport.FromProto(unknownVerdict); err == nil {
		t.Fatal("unknown protobuf verdict was accepted")
	}
}
