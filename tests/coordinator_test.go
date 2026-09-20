package tests

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"miniav/pkg/coordinator"
	processmanager "miniav/pkg/process"
	"miniav/pkg/protocol"
)

const coordinatorHelperEnvironment = "MINIAV_COORDINATOR_TEST_HELPER"

func TestCoordinatorHelper(t *testing.T) {
	switch os.Getenv(coordinatorHelperEnvironment) {
	case "wait":
		bufio.NewReader(os.Stdin).ReadString('\n')
	case "crash":
		bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(31)
	}
}

func TestWorkerCrashIsIsolatedAndPendingResultsBecomeUnavailable(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()

	survivor := startHelper(t, "wait")
	crashing := startHelper(t, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := actor.RegisterWorker(ctx, "scanner-a", survivor); err != nil {
		t.Fatal(err)
	}
	if err := actor.RegisterWorker(ctx, "scanner-b", crashing); err != nil {
		t.Fatal(err)
	}
	if err := actor.RegisterWorker(ctx, "scanner-a", survivor); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate worker error = %v", err)
	}
	if err := actor.TrackRequest(ctx, 101, []string{"scanner-a", "scanner-b"}); err != nil {
		t.Fatal(err)
	}
	if err := actor.RecordResult(ctx, 101, "scanner-a", protocol.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := actor.TrackRequest(ctx, 102, []string{"scanner-b"}); err != nil {
		t.Fatal(err)
	}

	if _, err := io.WriteString(crashing.Stdin(), "crash\n"); err != nil {
		t.Fatal(err)
	}
	crashing.Stdin().Close()

	snapshot := waitForWorkerState(t, ctx, actor, "scanner-b", coordinator.WorkerFailed)
	failed := snapshot.Workers["scanner-b"]
	if failed.ExitCode != 31 || failed.ExitError == "" {
		t.Fatalf("failed worker = %#v, want exit code 31 and an error", failed)
	}
	if survivorSnapshot := snapshot.Workers["scanner-a"]; survivorSnapshot.State != coordinator.WorkerStarting {
		t.Fatalf("surviving worker state = %q, want %q", survivorSnapshot.State, coordinator.WorkerStarting)
	}
	if got := snapshot.Requests[101].Results["scanner-a"]; got != protocol.VerdictClean {
		t.Fatalf("completed result = %q, want %q", got, protocol.VerdictClean)
	}
	if got := snapshot.Requests[101].Results["scanner-b"]; got != protocol.VerdictUnavailable {
		t.Fatalf("pending result = %q, want %q", got, protocol.VerdictUnavailable)
	}
	if got := snapshot.Requests[102].Results["scanner-b"]; got != protocol.VerdictUnavailable {
		t.Fatalf("pending result = %q, want %q", got, protocol.VerdictUnavailable)
	}

	if err := actor.TrackRequest(ctx, 103, []string{"scanner-a"}); err != nil {
		t.Fatalf("coordinator did not accept work after crash: %v", err)
	}
	if err := actor.TrackRequest(ctx, 104, []string{"scanner-b"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot after crash: %v", err)
	}
	if got := snapshot.Requests[104].Results["scanner-b"]; got != protocol.VerdictUnavailable {
		t.Fatalf("failed-worker assignment = %q, want %q", got, protocol.VerdictUnavailable)
	}

	if _, err := io.WriteString(survivor.Stdin(), "stop\n"); err != nil {
		t.Fatal(err)
	}
	survivor.Stdin().Close()
	waitForWorkerState(t, ctx, actor, "scanner-a", coordinator.WorkerFailed)
}

func TestCoordinatorValidatesCommandsAndCopiesSnapshots(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()
	child := startHelper(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := actor.RegisterWorker(ctx, "scanner-a", child); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{name: "zero request", run: func() error { return actor.TrackRequest(ctx, 0, []string{"scanner-a"}) }, want: "greater than zero"},
		{name: "no workers", run: func() error { return actor.TrackRequest(ctx, 1, nil) }, want: "at least one worker"},
		{name: "unknown worker", run: func() error { return actor.TrackRequest(ctx, 1, []string{"missing"}) }, want: "not registered"},
		{name: "duplicate assignment", run: func() error { return actor.TrackRequest(ctx, 1, []string{"scanner-a", "scanner-a"}) }, want: "duplicated"},
		{name: "unknown request result", run: func() error { return actor.RecordResult(ctx, 99, "scanner-a", protocol.VerdictClean) }, want: "not tracked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}

	if err := actor.TrackRequest(ctx, 1, []string{"scanner-a"}); err != nil {
		t.Fatal(err)
	}
	if err := actor.TrackRequest(ctx, 1, []string{"scanner-a"}); err == nil || !strings.Contains(err.Error(), "already tracked") {
		t.Fatalf("duplicate request error = %v", err)
	}
	if err := actor.RecordResult(ctx, 1, "missing", protocol.VerdictClean); err == nil || !strings.Contains(err.Error(), "not assigned") {
		t.Fatalf("unassigned worker error = %v", err)
	}
	if err := actor.RecordResult(ctx, 1, "scanner-a", protocol.Verdict("UNKNOWN")); err == nil || !strings.Contains(err.Error(), "invalid verdict") {
		t.Fatalf("invalid verdict error = %v", err)
	}
	if err := actor.RecordResult(ctx, 1, "scanner-a", protocol.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := actor.RecordResult(ctx, 1, "scanner-a", protocol.VerdictClean); err == nil || !strings.Contains(err.Error(), "already has a result") {
		t.Fatalf("duplicate result error = %v", err)
	}

	first, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Requests[1].Results["scanner-a"] = protocol.VerdictMalware
	delete(first.Workers, "scanner-a")
	second, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Requests[1].Results["scanner-a"] != protocol.VerdictClean {
		t.Fatalf("internal request state was mutated through snapshot: %#v", second.Requests[1])
	}
	if _, exists := second.Workers["scanner-a"]; !exists {
		t.Fatal("internal worker state was mutated through snapshot")
	}

	io.WriteString(child.Stdin(), "stop\n")
	child.Stdin().Close()
	waitForWorkerState(t, ctx, actor, "scanner-a", coordinator.WorkerFailed)
}

func TestCoordinatorHonorsContextCancellation(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)

	canceled, cancelCall := context.WithCancel(context.Background())
	cancelCall()
	if _, err := actor.Snapshot(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("snapshot error = %v, want context cancellation", err)
	}

	cancelActor()
	select {
	case <-actor.Done():
	case <-time.After(time.Second):
		t.Fatal("coordinator did not stop after context cancellation")
	}
	if _, err := actor.Snapshot(context.Background()); !errors.Is(err, coordinator.ErrStopped) {
		t.Fatalf("snapshot error = %v, want ErrStopped", err)
	}
}

func startHelper(t *testing.T, mode string) *processmanager.Child {
	t.Helper()
	t.Setenv(coordinatorHelperEnvironment, mode)
	child, err := processmanager.Start(os.Args[0], []string{"-test.run=^TestCoordinatorHelper$"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Stdin().Close()
	})
	return child
}

func waitForWorkerState(t *testing.T, ctx context.Context, actor *coordinator.Coordinator, workerID string, state coordinator.WorkerState) coordinator.Snapshot {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := actor.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if worker, exists := snapshot.Workers[workerID]; exists && worker.State == state {
			return snapshot
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker %q did not reach state %q: %v", workerID, state, ctx.Err())
		case <-ticker.C:
		}
	}
}
