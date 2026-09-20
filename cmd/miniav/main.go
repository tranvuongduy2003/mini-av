package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)

	app := cli{
		stdout: os.Stdout,
		stderr: os.Stderr,
		serve: func(ctx context.Context, configPath string) error {
			return serve(ctx, defaultCoreAddress, configPath)
		},
		request: func(ctx context.Context, request commandRequest) (commandResponse, error) {
			return sendCommand(ctx, defaultCoreAddress, request)
		},
	}

	exitCode := app.run(ctx, os.Args[1:])
	if exitCode == 2 {
		fmt.Fprint(os.Stderr, usage)
	}
	stop()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}
