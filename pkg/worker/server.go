package worker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"miniav/pkg/grpcprotocol"
	"miniav/pkg/ipc"
	"miniav/pkg/protocol"
	"miniav/pkg/workertransport"

	"google.golang.org/grpc"
)

func Serve(ctx context.Context, mode string, address string, config Config, bootstrap io.Writer) error {
	if mode != workertransport.Socket && mode != workertransport.GRPC {
		return fmt.Errorf("unsupported worker transport %q", mode)
	}
	if err := workertransport.ValidateLoopbackAddress(address); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for %s worker transport: %w", mode, err)
	}
	if err := workertransport.WriteBootstrap(bootstrap, listener.Addr().String()); err != nil {
		listener.Close()
		return err
	}
	switch mode {
	case workertransport.Socket:
		return serveSocket(ctx, listener, config)
	case workertransport.GRPC:
		return serveGRPC(ctx, listener, config)
	}
	return nil
}

func serveSocket(ctx context.Context, listener net.Listener, config Config) error {
	defer listener.Close()
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			listener.Close()
		case <-stop:
		}
	}()
	connection, err := listener.Accept()
	close(stop)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("accept worker socket: %w", err)
	}
	defer connection.Close()
	return Run(ctx, config, connection, connection)
}

type grpcService struct {
	grpcprotocol.UnimplementedScannerServer
	ctx      context.Context
	config   Config
	finished chan struct{}
	finish   sync.Once
}

func (service *grpcService) Exchange(stream grpcprotocol.Scanner_ExchangeServer) error {
	defer service.finish.Do(func() { close(service.finished) })
	receive := func() (protocol.Message, error) {
		payload, err := stream.Recv()
		if err != nil {
			return protocol.Message{}, err
		}
		return workertransport.FromProto(payload)
	}
	send := func(message protocol.Message) error {
		payload, err := workertransport.ToProto(message)
		if err != nil {
			return err
		}
		return stream.Send(payload)
	}
	err := RunSession(service.ctx, service.config, receive, send)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func serveGRPC(ctx context.Context, listener net.Listener, config Config) error {
	server := grpc.NewServer(grpc.MaxRecvMsgSize(ipc.MaxFrameSize), grpc.MaxSendMsgSize(ipc.MaxFrameSize))
	service := &grpcService{ctx: ctx, config: config, finished: make(chan struct{})}
	grpcprotocol.RegisterScannerServer(server, service)
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	select {
	case err := <-served:
		return err
	case <-service.finished:
		server.GracefulStop()
		err := <-served
		if errors.Is(err, grpc.ErrServerStopped) {
			return nil
		}
		return err
	case <-ctx.Done():
		server.Stop()
		<-served
		return ctx.Err()
	}
}
