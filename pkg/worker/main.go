package worker

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"miniav/pkg/workertransport"
)

func Main(scannerID string, transport string) int {
	flags := flag.NewFlagSet(scannerID, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	version := flags.String("worker-version", "1.0.0", "worker release version")
	signaturePath := flags.String("signatures", "", "initial signature database")
	delay := flags.Int("delay", 0, "scan response delay in milliseconds")
	crashOnScan := flags.Bool("crash-on-scan", false, "exit when a scan is received")
	failHealth := flags.Bool("fail-health", false, "report an unhealthy status")
	listenAddress := flags.String("listen", "127.0.0.1:0", "loopback listener address for socket or gRPC")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "worker does not accept positional arguments")
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	config := Config{
		ScannerID:     scannerID,
		WorkerVersion: *version,
		SignaturePath: *signaturePath,
		Delay:         time.Duration(*delay) * time.Millisecond,
		CrashOnScan:   *crashOnScan,
		FailHealth:    *failHealth,
		Logger:        logger,
	}
	var err error
	if transport == workertransport.Stdio {
		err = Run(context.Background(), config, os.Stdin, os.Stdout)
	} else {
		err = Serve(context.Background(), transport, *listenAddress, config, os.Stdout)
	}
	if err != nil {
		logger.Error("worker stopped", "error", err)
		return 1
	}
	return 0
}
