package workertransport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"

	"miniav/pkg/grpcprotocol"
	"miniav/pkg/ipc"
	"miniav/pkg/protocol"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	Stdio  = "stdio"
	Socket = "socket"
	GRPC   = "grpc"
)

type Connection interface {
	Send(protocol.Message) error
	Receive() (protocol.Message, error)
	Close() error
}

type Bootstrap struct {
	Address string `json:"address"`
}

type streamConnection struct {
	decoder *ipc.Decoder
	encoder *ipc.Encoder
	closers []io.Closer
}

func NewStream(reader io.Reader, writer io.Writer, closers ...io.Closer) Connection {
	return &streamConnection{decoder: ipc.NewDecoder(reader), encoder: ipc.NewEncoder(writer), closers: closers}
}

func (connection *streamConnection) Send(message protocol.Message) error {
	return connection.encoder.Encode(message)
}

func (connection *streamConnection) Receive() (protocol.Message, error) {
	return connection.decoder.Decode()
}

func (connection *streamConnection) Close() error {
	var result error
	for _, closer := range connection.closers {
		if err := closer.Close(); err != nil && result == nil {
			result = err
		}
	}
	return result
}

func ReadBootstrap(reader io.Reader) (Bootstrap, error) {
	frame, err := bufio.NewReader(io.LimitReader(reader, ipc.MaxFrameSize+1)).ReadBytes('\n')
	if len(frame) > ipc.MaxFrameSize {
		return Bootstrap{}, errors.New("worker bootstrap exceeds size limit")
	}
	if err != nil {
		return Bootstrap{}, fmt.Errorf("read worker bootstrap: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(frame)))
	decoder.DisallowUnknownFields()
	var bootstrap Bootstrap
	if err := decoder.Decode(&bootstrap); err != nil {
		return Bootstrap{}, fmt.Errorf("decode worker bootstrap: %w", err)
	}
	if strings.TrimSpace(bootstrap.Address) == "" {
		return Bootstrap{}, errors.New("worker bootstrap address must not be empty")
	}
	return bootstrap, nil
}

func WriteBootstrap(writer io.Writer, address string) error {
	if strings.TrimSpace(address) == "" {
		return errors.New("worker bootstrap address must not be empty")
	}
	return json.NewEncoder(writer).Encode(Bootstrap{Address: address})
}

func Dial(ctx context.Context, mode string, address string) (Connection, error) {
	if err := ValidateLoopbackAddress(address); err != nil {
		return nil, err
	}
	switch mode {
	case Socket:
		connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return nil, fmt.Errorf("dial worker socket %s: %w", address, err)
		}
		return NewStream(connection, connection, connection), nil
	case GRPC:
		client, err := grpc.DialContext(ctx, address,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(ipc.MaxFrameSize),
				grpc.MaxCallSendMsgSize(ipc.MaxFrameSize),
			),
			grpc.WithBlock(),
		)
		if err != nil {
			return nil, fmt.Errorf("dial worker gRPC %s: %w", address, err)
		}
		streamContext, cancel := context.WithCancel(context.Background())
		stream, err := grpcprotocol.NewScannerClient(client).Exchange(streamContext)
		if err != nil {
			cancel()
			client.Close()
			return nil, fmt.Errorf("open worker gRPC stream: %w", err)
		}
		return &grpcConnection{client: client, stream: stream, cancel: cancel}, nil
	default:
		return nil, fmt.Errorf("unsupported worker transport %q", mode)
	}
}

func ValidateLoopbackAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid worker address %q: %w", address, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("worker address %q must use a literal loopback IP", address)
	}
	return nil
}

type grpcConnection struct {
	client *grpc.ClientConn
	stream grpcprotocol.Scanner_ExchangeClient
	cancel context.CancelFunc
}

func (connection *grpcConnection) Send(message protocol.Message) error {
	payload, err := ToProto(message)
	if err != nil {
		return err
	}
	return connection.stream.Send(payload)
}

func (connection *grpcConnection) Receive() (protocol.Message, error) {
	payload, err := connection.stream.Recv()
	if err != nil {
		return protocol.Message{}, err
	}
	return FromProto(payload)
}

func (connection *grpcConnection) Close() error {
	connection.cancel()
	return connection.client.Close()
}
