package workertransport

import (
	"errors"
	"fmt"
	"math"

	"miniav/pkg/grpcprotocol"
	"miniav/pkg/protocol"

	"google.golang.org/protobuf/proto"
)

func ToProto(message protocol.Message) (*grpcprotocol.Message, error) {
	if err := protocol.Validate(message); err != nil {
		return nil, err
	}
	payload := &grpcprotocol.Message{
		Version:       int32(message.Version),
		Type:          message.Type,
		RequestId:     message.RequestID,
		ScannerId:     message.ScannerID,
		WorkerVersion: message.WorkerVersion,
		Capabilities:  append([]string(nil), message.Capabilities...),
		FilePath:      message.FilePath,
		SignaturePath: message.SignaturePath,
		Status:        message.Status,
		Verdict:       verdictToProto(message.Verdict),
		Signature:     message.Signature,
		Error:         message.Error,
	}
	if message.Type == protocol.TypeScanResult {
		payload.DurationMs = proto.Int64(message.DurationMs)
	}
	if message.Type == protocol.TypeReloadResult {
		if message.SignatureCount > math.MaxInt32 {
			return nil, errors.New("RELOAD_RESULT signature_count exceeds protobuf int32")
		}
		payload.Success = proto.Bool(message.Success)
		if message.Success || message.SignatureCount != 0 {
			payload.SignatureCount = proto.Int32(int32(message.SignatureCount))
		}
	}
	return payload, nil
}

func FromProto(payload *grpcprotocol.Message) (protocol.Message, error) {
	if payload == nil {
		return protocol.Message{}, errors.New("gRPC message must not be nil")
	}
	if err := validateProtoPresence(payload); err != nil {
		return protocol.Message{}, err
	}
	verdict, err := verdictFromProto(payload.Verdict)
	if err != nil {
		return protocol.Message{}, err
	}
	message := protocol.Message{
		Version:       int(payload.Version),
		Type:          payload.Type,
		RequestID:     payload.RequestId,
		ScannerID:     payload.ScannerId,
		WorkerVersion: payload.WorkerVersion,
		Capabilities:  append([]string(nil), payload.Capabilities...),
		FilePath:      payload.FilePath,
		SignaturePath: payload.SignaturePath,
		Status:        payload.Status,
		Verdict:       verdict,
		Signature:     payload.Signature,
		Error:         payload.Error,
	}
	if payload.DurationMs != nil {
		message.DurationMs = *payload.DurationMs
	}
	if payload.Success != nil {
		message.Success = *payload.Success
	}
	if payload.SignatureCount != nil {
		message.SignatureCount = int(*payload.SignatureCount)
	}
	if err := protocol.Validate(message); err != nil {
		return protocol.Message{}, err
	}
	return message, nil
}

func validateProtoPresence(message *grpcprotocol.Message) error {
	if message.Type == protocol.TypeScanResult && message.DurationMs == nil {
		return errors.New("SCAN_RESULT requires duration_ms")
	}
	if message.Type == protocol.TypeReloadResult {
		if message.Success == nil {
			return errors.New("RELOAD_RESULT requires success")
		}
		if *message.Success && message.SignatureCount == nil {
			return errors.New("RELOAD_RESULT requires signature_count")
		}
	}
	if message.Type != protocol.TypeScanResult && message.DurationMs != nil {
		return fmt.Errorf("%s does not allow field duration_ms", message.Type)
	}
	if message.Type != protocol.TypeReloadResult && (message.Success != nil || message.SignatureCount != nil) {
		return fmt.Errorf("%s does not allow reload result fields", message.Type)
	}
	return nil
}

func verdictToProto(verdict protocol.Verdict) grpcprotocol.Verdict {
	switch verdict {
	case protocol.VerdictClean:
		return grpcprotocol.Verdict_VERDICT_CLEAN
	case protocol.VerdictMalware:
		return grpcprotocol.Verdict_VERDICT_MALWARE
	case protocol.VerdictError:
		return grpcprotocol.Verdict_VERDICT_ERROR
	case protocol.VerdictTimeout:
		return grpcprotocol.Verdict_VERDICT_TIMEOUT
	case protocol.VerdictUnavailable:
		return grpcprotocol.Verdict_VERDICT_UNAVAILABLE
	default:
		return grpcprotocol.Verdict_VERDICT_UNSPECIFIED
	}
}

func verdictFromProto(verdict grpcprotocol.Verdict) (protocol.Verdict, error) {
	switch verdict {
	case grpcprotocol.Verdict_VERDICT_UNSPECIFIED:
		return "", nil
	case grpcprotocol.Verdict_VERDICT_CLEAN:
		return protocol.VerdictClean, nil
	case grpcprotocol.Verdict_VERDICT_MALWARE:
		return protocol.VerdictMalware, nil
	case grpcprotocol.Verdict_VERDICT_ERROR:
		return protocol.VerdictError, nil
	case grpcprotocol.Verdict_VERDICT_TIMEOUT:
		return protocol.VerdictTimeout, nil
	case grpcprotocol.Verdict_VERDICT_UNAVAILABLE:
		return protocol.VerdictUnavailable, nil
	default:
		return "", fmt.Errorf("unknown protobuf verdict %d", verdict)
	}
}
