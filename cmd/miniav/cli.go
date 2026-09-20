package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

const usage = `usage:
  miniav serve --config <path>
  miniav scan <file>
  miniav status
  miniav reload <scanner-id> <path>
  miniav update --manifest <path>
  miniav shutdown
`

type invocation struct {
	serve      bool
	configPath string
	request    commandRequest
}

type cli struct {
	stdout  io.Writer
	stderr  io.Writer
	serve   func(context.Context, string) error
	request func(context.Context, commandRequest) (commandResponse, error)
}

func (c cli) run(ctx context.Context, args []string) int {
	invocation, err := parseInvocation(args)
	if err != nil {
		fmt.Fprintln(c.stderr, err)
		return 2
	}

	if invocation.serve {
		if err := c.serve(ctx, invocation.configPath); err != nil {
			fmt.Fprintln(c.stderr, err)
			return 1
		}
		return 0
	}

	response, err := c.request(ctx, invocation.request)
	if err != nil {
		fmt.Fprintln(c.stderr, err)
		return 1
	}
	if !response.Success {
		if response.Error == "" {
			response.Error = "core returned an unsuccessful response"
		}
		fmt.Fprintln(c.stderr, response.Error)
		return 1
	}
	if response.Output != "" {
		fmt.Fprintln(c.stdout, response.Output)
	}
	return 0
}

func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 {
		return invocation{}, errors.New("a command is required")
	}

	switch args[0] {
	case "serve":
		configPath, err := requiredFlag(args[1:], "config", "serve")
		if err != nil {
			return invocation{}, err
		}
		return invocation{serve: true, configPath: configPath}, nil
	case "scan":
		if len(args) != 2 {
			return invocation{}, errors.New("scan requires exactly one file path")
		}
		if strings.TrimSpace(args[1]) == "" {
			return invocation{}, errors.New("scan requires a non-empty file path")
		}
		return requestInvocation("scan", args[1]), nil
	case "status":
		if len(args) != 1 {
			return invocation{}, errors.New("status does not accept arguments")
		}
		return requestInvocation("status"), nil
	case "reload":
		if len(args) != 3 {
			return invocation{}, errors.New("reload requires a scanner ID and signature path")
		}
		if strings.TrimSpace(args[1]) == "" || strings.TrimSpace(args[2]) == "" {
			return invocation{}, errors.New("reload requires a non-empty scanner ID and signature path")
		}
		return requestInvocation("reload", args[1], args[2]), nil
	case "update":
		manifestPath, err := requiredFlag(args[1:], "manifest", "update")
		if err != nil {
			return invocation{}, err
		}
		return requestInvocation("update", manifestPath), nil
	case "shutdown":
		if len(args) != 1 {
			return invocation{}, errors.New("shutdown does not accept arguments")
		}
		return requestInvocation("shutdown"), nil
	default:
		return invocation{}, fmt.Errorf("unknown command %q", args[0])
	}
}

func requiredFlag(args []string, name string, command string) (string, error) {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	value := flags.String(name, "", "")
	if err := flags.Parse(args); err != nil {
		return "", err
	}
	if flags.NArg() != 0 {
		return "", fmt.Errorf("%s does not accept positional arguments", command)
	}
	if strings.TrimSpace(*value) == "" {
		return "", fmt.Errorf("%s requires --%s <path>", command, name)
	}
	return *value, nil
}

func requestInvocation(command string, args ...string) invocation {
	return invocation{request: commandRequest{Command: command, Args: args}}
}
