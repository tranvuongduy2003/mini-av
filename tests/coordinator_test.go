package tests

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
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

	survivorKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "1.0.0"}
	crashingKey := coordinator.WorkerKey{ScannerID: "scanner-b", Version: "1.0.0"}
	survivor := startCoordinatorHelper(t, "wait")
	crashing := startCoordinatorHelper(t, "crash")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	registerActiveWorker(t, ctx, actor, survivorKey, survivor)
	registerActiveWorker(t, ctx, actor, crashingKey, crashing)
	if err := actor.TrackRequest(ctx, 101, []coordinator.WorkerKey{survivorKey, crashingKey}); err != nil {
		t.Fatal(err)
	}
	if err := actor.RecordResult(ctx, 101, survivorKey, protocol.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := actor.TrackRequest(ctx, 102, []coordinator.WorkerKey{crashingKey}); err != nil {
		t.Fatal(err)
	}

	if _, err := io.WriteString(crashing.Stdin(), "crash\n"); err != nil {
		t.Fatal(err)
	}
	crashing.Stdin().Close()

	snapshot := waitForWorkerState(t, ctx, actor, crashingKey, coordinator.WorkerFailed)
	failed := snapshot.Workers[crashingKey]
	if failed.ExitCode != 31 || failed.ExitError == "" || failed.FailureReason == "" {
		t.Fatalf("failed worker = %#v, want crash details", failed)
	}
	if survivorSnapshot := snapshot.Workers[survivorKey]; survivorSnapshot.State != coordinator.WorkerActive {
		t.Fatalf("surviving worker state = %q, want %q", survivorSnapshot.State, coordinator.WorkerActive)
	}
	if snapshot.Active["scanner-a"] != survivorKey {
		t.Fatalf("scanner-a route = %#v, want %#v", snapshot.Active["scanner-a"], survivorKey)
	}
	if _, exists := snapshot.Active["scanner-b"]; exists {
		t.Fatalf("scanner-b retained failed active route: %#v", snapshot.Active)
	}
	if got := snapshot.Requests[101].Results[survivorKey]; got != protocol.VerdictClean {
		t.Fatalf("completed result = %q, want %q", got, protocol.VerdictClean)
	}
	if got := snapshot.Requests[101].Results[crashingKey]; got != protocol.VerdictUnavailable {
		t.Fatalf("pending result = %q, want %q", got, protocol.VerdictUnavailable)
	}
	if got := snapshot.Requests[102].Results[crashingKey]; got != protocol.VerdictUnavailable {
		t.Fatalf("pending result = %q, want %q", got, protocol.VerdictUnavailable)
	}

	if err := actor.TrackRequest(ctx, 103, []coordinator.WorkerKey{survivorKey}); err != nil {
		t.Fatalf("coordinator did not accept work after crash: %v", err)
	}
	if err := actor.TrackRequest(ctx, 104, []coordinator.WorkerKey{crashingKey}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Requests[104].Results[crashingKey]; got != protocol.VerdictUnavailable {
		t.Fatalf("failed-worker assignment = %q, want %q", got, protocol.VerdictUnavailable)
	}
}

func TestCoordinatorActivatesCandidateAndDrainsPreviousWorker(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "1.0.0"}
	newKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "2.0.0"}
	oldWorker := startCoordinatorHelper(t, "wait")
	newWorker := startCoordinatorHelper(t, "wait")
	registerActiveWorker(t, ctx, actor, oldKey, oldWorker)
	if err := actor.TrackRequest(ctx, 201, []coordinator.WorkerKey{oldKey}); err != nil {
		t.Fatal(err)
	}
	if err := actor.RegisterWorker(ctx, newKey, newWorker); err != nil {
		t.Fatal(err)
	}
	if err := actor.ActivateWorker(ctx, newKey); err == nil || !strings.Contains(err.Error(), "STARTING") {
		t.Fatalf("activation before health error = %v", err)
	}
	if err := actor.MarkHealthy(ctx, newKey); err != nil {
		t.Fatal(err)
	}
	if err := actor.ActivateWorker(ctx, newKey); err != nil {
		t.Fatal(err)
	}

	snapshot, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Active["scanner-a"] != newKey {
		t.Fatalf("active route = %#v, want %#v", snapshot.Active["scanner-a"], newKey)
	}
	if snapshot.Workers[oldKey].State != coordinator.WorkerDraining || snapshot.Workers[newKey].State != coordinator.WorkerActive {
		t.Fatalf("worker states after activation = %#v", snapshot.Workers)
	}
	if err := actor.RetireWorker(ctx, oldKey); err == nil || !strings.Contains(err.Error(), "in-flight request 201") {
		t.Fatalf("retire with in-flight request error = %v", err)
	}
	if err := actor.RecordResult(ctx, 201, oldKey, protocol.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := actor.RetireWorker(ctx, oldKey); err != nil {
		t.Fatal(err)
	}
	snapshot, err = actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Workers[oldKey].State != coordinator.WorkerRetired {
		t.Fatalf("old worker state = %q, want %q", snapshot.Workers[oldKey].State, coordinator.WorkerRetired)
	}
}

func TestCoordinatorRollsBackPreActivationFailures(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "1.0.0"}
	oldWorker := startCoordinatorHelper(t, "wait")
	registerActiveWorker(t, ctx, actor, oldKey, oldWorker)

	missing := filepath.Join(t.TempDir(), "missing-worker")
	if child, err := processmanager.Start(missing, nil, io.Discard); err == nil || child != nil {
		t.Fatalf("missing candidate returned child %#v and error %v", child, err)
	}
	assertActiveRoute(t, ctx, actor, oldKey)

	healthFailureKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "2.0.0"}
	healthFailure := startCoordinatorHelper(t, "wait")
	if err := actor.RegisterWorker(ctx, healthFailureKey, healthFailure); err != nil {
		t.Fatal(err)
	}
	if err := actor.MarkHealthy(ctx, healthFailureKey); err != nil {
		t.Fatal(err)
	}
	if err := actor.FailWorker(ctx, healthFailureKey, "health check failed"); err != nil {
		t.Fatal(err)
	}
	snapshot := waitForWorkerState(t, ctx, actor, healthFailureKey, coordinator.WorkerFailed)
	if snapshot.Workers[healthFailureKey].FailureReason != "health check failed" {
		t.Fatalf("failure reason = %q", snapshot.Workers[healthFailureKey].FailureReason)
	}
	assertActiveRoute(t, ctx, actor, oldKey)

	timeoutKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "3.0.0"}
	timedOut := startCoordinatorHelper(t, "wait")
	if err := actor.RegisterWorker(ctx, timeoutKey, timedOut); err != nil {
		t.Fatal(err)
	}
	if err := actor.FailWorker(ctx, timeoutKey, "startup timeout"); err != nil {
		t.Fatal(err)
	}
	waitForWorkerState(t, ctx, actor, timeoutKey, coordinator.WorkerFailed)
	assertActiveRoute(t, ctx, actor, oldKey)

	crashKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "4.0.0"}
	crashing := startCoordinatorHelper(t, "crash")
	if err := actor.RegisterWorker(ctx, crashKey, crashing); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(crashing.Stdin(), "crash\n"); err != nil {
		t.Fatal(err)
	}
	crashing.Stdin().Close()
	waitForWorkerState(t, ctx, actor, crashKey, coordinator.WorkerFailed)
	assertActiveRoute(t, ctx, actor, oldKey)
}

func TestCoordinatorDoesNotRollbackAfterActivation(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	oldKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "1.0.0"}
	newKey := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "2.0.0"}
	oldWorker := startCoordinatorHelper(t, "wait")
	newWorker := startCoordinatorHelper(t, "crash")
	registerActiveWorker(t, ctx, actor, oldKey, oldWorker)
	if err := actor.RegisterWorker(ctx, newKey, newWorker); err != nil {
		t.Fatal(err)
	}
	if err := actor.MarkHealthy(ctx, newKey); err != nil {
		t.Fatal(err)
	}
	if err := actor.ActivateWorker(ctx, newKey); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(newWorker.Stdin(), "crash\n"); err != nil {
		t.Fatal(err)
	}
	newWorker.Stdin().Close()
	snapshot := waitForWorkerState(t, ctx, actor, newKey, coordinator.WorkerFailed)
	if _, exists := snapshot.Active["scanner-a"]; exists {
		t.Fatalf("failed active candidate retained a route: %#v", snapshot.Active)
	}
	if snapshot.Workers[oldKey].State != coordinator.WorkerDraining {
		t.Fatalf("old worker state = %q, want no post-activation rollback from %q", snapshot.Workers[oldKey].State, coordinator.WorkerDraining)
	}
}

func TestCoordinatorValidatesCommandsAndCopiesSnapshots(t *testing.T) {
	actorContext, cancelActor := context.WithCancel(context.Background())
	actor := coordinator.Start(actorContext)
	defer func() {
		cancelActor()
		<-actor.Done()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := coordinator.WorkerKey{ScannerID: "scanner-a", Version: "1.0.0"}
	child := startCoordinatorHelper(t, "wait")

	if err := actor.RegisterWorker(ctx, coordinator.WorkerKey{}, child); err == nil || !strings.Contains(err.Error(), "scanner ID") {
		t.Fatalf("empty key error = %v", err)
	}
	if err := actor.RegisterWorker(ctx, key, nil); err == nil || !strings.Contains(err.Error(), "must not be nil") {
		t.Fatalf("nil child error = %v", err)
	}
	registerActiveWorker(t, ctx, actor, key, child)
	if err := actor.RegisterWorker(ctx, key, child); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate worker error = %v", err)
	}

	tests := []struct {
		name string
		run  func() error
		want string
	}{
		{name: "zero request", run: func() error { return actor.TrackRequest(ctx, 0, []coordinator.WorkerKey{key}) }, want: "greater than zero"},
		{name: "no workers", run: func() error { return actor.TrackRequest(ctx, 1, nil) }, want: "at least one worker"},
		{name: "unknown worker", run: func() error {
			return actor.TrackRequest(ctx, 1, []coordinator.WorkerKey{{ScannerID: "missing", Version: "1"}})
		}, want: "not registered"},
		{name: "duplicate assignment", run: func() error { return actor.TrackRequest(ctx, 1, []coordinator.WorkerKey{key, key}) }, want: "duplicated"},
		{name: "unknown request result", run: func() error { return actor.RecordResult(ctx, 99, key, protocol.VerdictClean) }, want: "not tracked"},
		{name: "healthy active worker", run: func() error { return actor.MarkHealthy(ctx, key) }, want: "cannot transition"},
		{name: "fail active worker", run: func() error { return actor.FailWorker(ctx, key, "failed") }, want: "cannot fail before activation"},
		{name: "retire active worker", run: func() error { return actor.RetireWorker(ctx, key) }, want: "cannot transition"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to contain %q", err, test.want)
			}
		})
	}

	if err := actor.TrackRequest(ctx, 1, []coordinator.WorkerKey{key}); err != nil {
		t.Fatal(err)
	}
	if err := actor.TrackRequest(ctx, 1, []coordinator.WorkerKey{key}); err == nil || !strings.Contains(err.Error(), "already tracked") {
		t.Fatalf("duplicate request error = %v", err)
	}
	missing := coordinator.WorkerKey{ScannerID: "missing", Version: "1"}
	if err := actor.RecordResult(ctx, 1, missing, protocol.VerdictClean); err == nil || !strings.Contains(err.Error(), "not assigned") {
		t.Fatalf("unassigned worker error = %v", err)
	}
	if err := actor.RecordResult(ctx, 1, key, protocol.Verdict("UNKNOWN")); err == nil || !strings.Contains(err.Error(), "invalid verdict") {
		t.Fatalf("invalid verdict error = %v", err)
	}
	if err := actor.RecordResult(ctx, 1, key, protocol.VerdictClean); err != nil {
		t.Fatal(err)
	}
	if err := actor.RecordResult(ctx, 1, key, protocol.VerdictClean); err == nil || !strings.Contains(err.Error(), "already has a result") {
		t.Fatalf("duplicate result error = %v", err)
	}

	first, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first.Requests[1].Results[key] = protocol.VerdictMalware
	delete(first.Workers, key)
	delete(first.Active, "scanner-a")
	second, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Requests[1].Results[key] != protocol.VerdictClean {
		t.Fatalf("internal request state was mutated through snapshot: %#v", second.Requests[1])
	}
	if _, exists := second.Workers[key]; !exists {
		t.Fatal("internal worker state was mutated through snapshot")
	}
	if second.Active["scanner-a"] != key {
		t.Fatal("internal routing state was mutated through snapshot")
	}
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

func startCoordinatorHelper(t *testing.T, mode string) *processmanager.Child {
	t.Helper()
	t.Setenv(coordinatorHelperEnvironment, mode)
	child, err := processmanager.Start(os.Args[0], []string{"-test.run=^TestCoordinatorHelper$"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		child.Stdin().Close()
		child.Terminate()
	})
	return child
}

func registerActiveWorker(t *testing.T, ctx context.Context, actor *coordinator.Coordinator, key coordinator.WorkerKey, child *processmanager.Child) {
	t.Helper()
	if err := actor.RegisterWorker(ctx, key, child); err != nil {
		t.Fatal(err)
	}
	if err := actor.MarkHealthy(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := actor.ActivateWorker(ctx, key); err != nil {
		t.Fatal(err)
	}
}

func assertActiveRoute(t *testing.T, ctx context.Context, actor *coordinator.Coordinator, key coordinator.WorkerKey) {
	t.Helper()
	snapshot, err := actor.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Active[key.ScannerID] != key || snapshot.Workers[key].State != coordinator.WorkerActive {
		t.Fatalf("active route after candidate failure = %#v; workers = %#v", snapshot.Active, snapshot.Workers)
	}
}

func waitForWorkerState(t *testing.T, ctx context.Context, actor *coordinator.Coordinator, key coordinator.WorkerKey, state coordinator.WorkerState) coordinator.Snapshot {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := actor.Snapshot(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if worker, exists := snapshot.Workers[key]; exists && worker.State == state {
			return snapshot
		}
		select {
		case <-ctx.Done():
			t.Fatalf("worker %q did not reach state %q: %v", key, state, ctx.Err())
		case <-ticker.C:
		}
	}
}
