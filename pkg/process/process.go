package process

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

type Exit struct {
	PID  int
	Code int
	Err  error
}

type Child struct {
	pid          int
	process      *os.Process
	stdin        io.WriteCloser
	stdout       io.ReadCloser
	exited       <-chan Exit
	terminate    sync.Once
	terminateErr error
}

func Start(executable string, args []string, stderr io.Writer) (*Child, error) {
	if strings.TrimSpace(executable) == "" {
		return nil, fmt.Errorf("worker executable must not be empty")
	}

	command := exec.Command(executable, args...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open worker stdin: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("open worker stdout: %w", err)
	}
	if stderr == nil {
		stderr = io.Discard
	}
	command.Stderr = stderr

	if err := command.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("start worker %q: %w", executable, err)
	}

	exited := make(chan Exit, 1)
	child := &Child{
		pid:     command.Process.Pid,
		process: command.Process,
		stdin:   stdin,
		stdout:  stdout,
		exited:  exited,
	}
	go func() {
		err := command.Wait()
		exited <- Exit{
			PID:  command.Process.Pid,
			Code: command.ProcessState.ExitCode(),
			Err:  err,
		}
		close(exited)
	}()
	return child, nil
}

func (child *Child) PID() int {
	return child.pid
}

func (child *Child) Stdin() io.WriteCloser {
	return child.stdin
}

func (child *Child) Stdout() io.ReadCloser {
	return child.stdout
}

func (child *Child) Exited() <-chan Exit {
	return child.exited
}

func (child *Child) Terminate() error {
	child.terminate.Do(func() {
		child.terminateErr = child.process.Kill()
		if errors.Is(child.terminateErr, os.ErrProcessDone) {
			child.terminateErr = nil
		}
	})
	return child.terminateErr
}
