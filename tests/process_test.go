package tests

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	processmanager "miniav/pkg/process"
)

const processHelperEnvironment = "MINIAV_PROCESS_TEST_HELPER"

func TestProcessHelper(t *testing.T) {
	switch os.Getenv(processHelperEnvironment) {
	case "echo":
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			os.Exit(20)
		}
		fmt.Fprint(os.Stdout, line)
		fmt.Fprintln(os.Stderr, "diagnostic")
	case "crash":
		os.Exit(23)
	}
}

func TestStartConnectsPipesAndReportsExit(t *testing.T) {
	t.Setenv(processHelperEnvironment, "echo")
	var stderr bytes.Buffer
	child, err := processmanager.Start(os.Args[0], []string{"-test.run=^TestProcessHelper$"}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Stdin().Close()
	})
	if child.PID() <= 0 {
		t.Fatalf("PID = %d, want a positive value", child.PID())
	}
	if _, err := io.WriteString(child.Stdin(), "ready\n"); err != nil {
		t.Fatal(err)
	}
	if err := child.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(child.Stdout()).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "ready\n" {
		t.Fatalf("stdout = %q, want %q", line, "ready\n")
	}

	exit := waitForExit(t, child.Exited())
	if exit.PID != child.PID() || exit.Code != 0 || exit.Err != nil {
		t.Fatalf("exit = %#v, want PID %d with code 0 and no error", exit, child.PID())
	}
	if stderr.String() != "diagnostic\n" {
		t.Fatalf("stderr = %q, want %q", stderr.String(), "diagnostic\n")
	}
	if _, ok := <-child.Exited(); ok {
		t.Fatal("exit channel remained open after its single result")
	}
}

func TestStartReportsCrash(t *testing.T) {
	t.Setenv(processHelperEnvironment, "crash")
	child, err := processmanager.Start(os.Args[0], []string{"-test.run=^TestProcessHelper$"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	exit := waitForExit(t, child.Exited())
	if exit.PID != child.PID() || exit.Code != 23 || exit.Err == nil {
		t.Fatalf("exit = %#v, want PID %d with code 23 and an error", exit, child.PID())
	}
}

func TestStartRejectsInvalidExecutable(t *testing.T) {
	if child, err := processmanager.Start(" ", nil, io.Discard); err == nil || child != nil {
		t.Fatalf("empty executable returned child %#v and error %v", child, err)
	}

	missing := filepath.Join(t.TempDir(), "missing-worker")
	child, err := processmanager.Start(missing, nil, io.Discard)
	if err == nil || child != nil {
		t.Fatalf("missing executable returned child %#v and error %v", child, err)
	}
	if !strings.Contains(err.Error(), "start worker") {
		t.Fatalf("error = %q, want start worker context", err)
	}
}

func waitForExit(t *testing.T, exits <-chan processmanager.Exit) processmanager.Exit {
	t.Helper()
	select {
	case exit, ok := <-exits:
		if !ok {
			t.Fatal("exit channel closed without a result")
		}
		return exit
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker exit")
		return processmanager.Exit{}
	}
}
