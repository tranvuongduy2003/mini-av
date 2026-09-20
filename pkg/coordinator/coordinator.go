package coordinator

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"miniav/pkg/process"
	"miniav/pkg/protocol"
)

var ErrStopped = errors.New("coordinator stopped")

type WorkerState string

const (
	WorkerStarting WorkerState = "STARTING"
	WorkerFailed   WorkerState = "FAILED"
)

type WorkerSnapshot struct {
	ID        string
	PID       int
	State     WorkerState
	ExitCode  int
	ExitError string
}

type RequestSnapshot struct {
	ID      uint64
	Results map[string]protocol.Verdict
}

type Snapshot struct {
	Workers  map[string]WorkerSnapshot
	Requests map[uint64]RequestSnapshot
}

type Coordinator struct {
	commands chan command
	done     chan struct{}
}

type worker struct {
	id        string
	pid       int
	state     WorkerState
	exitCode  int
	exitError string
}

type request struct {
	id      uint64
	results map[string]protocol.Verdict
}

type command interface {
	isCommand()
}

type registerWorkerCommand struct {
	id    string
	child *process.Child
	reply chan error
}

func (registerWorkerCommand) isCommand() {}

type trackRequestCommand struct {
	id        uint64
	workerIDs []string
	reply     chan error
}

func (trackRequestCommand) isCommand() {}

type recordResultCommand struct {
	requestID uint64
	workerID  string
	verdict   protocol.Verdict
	reply     chan error
}

func (recordResultCommand) isCommand() {}

type snapshotCommand struct {
	reply chan snapshotResult
}

func (snapshotCommand) isCommand() {}

type workerExitedCommand struct {
	workerID string
	exit     process.Exit
}

func (workerExitedCommand) isCommand() {}

type snapshotResult struct {
	snapshot Snapshot
	err      error
}

func Start(ctx context.Context) *Coordinator {
	coordinator := &Coordinator{
		commands: make(chan command),
		done:     make(chan struct{}),
	}
	go coordinator.run(ctx)
	return coordinator
}

func (coordinator *Coordinator) RegisterWorker(ctx context.Context, id string, child *process.Child) error {
	reply := make(chan error, 1)
	command := registerWorkerCommand{id: id, child: child, reply: reply}
	if err := coordinator.send(ctx, command); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) TrackRequest(ctx context.Context, id uint64, workerIDs []string) error {
	reply := make(chan error, 1)
	command := trackRequestCommand{id: id, workerIDs: append([]string(nil), workerIDs...), reply: reply}
	if err := coordinator.send(ctx, command); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) RecordResult(ctx context.Context, requestID uint64, workerID string, verdict protocol.Verdict) error {
	reply := make(chan error, 1)
	command := recordResultCommand{
		requestID: requestID,
		workerID:  workerID,
		verdict:   verdict,
		reply:     reply,
	}
	if err := coordinator.send(ctx, command); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) Snapshot(ctx context.Context) (Snapshot, error) {
	reply := make(chan snapshotResult, 1)
	if err := coordinator.send(ctx, snapshotCommand{reply: reply}); err != nil {
		return Snapshot{}, err
	}
	select {
	case result := <-reply:
		return result.snapshot, result.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-coordinator.done:
		return Snapshot{}, ErrStopped
	}
}

func (coordinator *Coordinator) Done() <-chan struct{} {
	return coordinator.done
}

func (coordinator *Coordinator) send(ctx context.Context, command command) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case coordinator.commands <- command:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-coordinator.done:
		return ErrStopped
	}
}

func (coordinator *Coordinator) waitError(ctx context.Context, reply <-chan error) error {
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-coordinator.done:
		return ErrStopped
	}
}

func (coordinator *Coordinator) run(ctx context.Context) {
	defer close(coordinator.done)
	workers := make(map[string]*worker)
	requests := make(map[uint64]*request)

	for {
		select {
		case <-ctx.Done():
			return
		case received := <-coordinator.commands:
			switch command := received.(type) {
			case registerWorkerCommand:
				command.reply <- coordinator.registerWorker(workers, command)
			case trackRequestCommand:
				command.reply <- trackRequest(workers, requests, command)
			case recordResultCommand:
				command.reply <- recordResult(requests, command)
			case snapshotCommand:
				command.reply <- snapshotResult{snapshot: makeSnapshot(workers, requests)}
			case workerExitedCommand:
				recordWorkerExit(workers, requests, command)
			}
		}
	}
}

func (coordinator *Coordinator) registerWorker(workers map[string]*worker, command registerWorkerCommand) error {
	if strings.TrimSpace(command.id) == "" {
		return errors.New("worker ID must not be empty")
	}
	if command.child == nil {
		return errors.New("worker child must not be nil")
	}
	if _, exists := workers[command.id]; exists {
		return fmt.Errorf("worker %q is already registered", command.id)
	}

	workers[command.id] = &worker{
		id:    command.id,
		pid:   command.child.PID(),
		state: WorkerStarting,
	}
	go coordinator.monitor(command.id, command.child.Exited())
	return nil
}

func (coordinator *Coordinator) monitor(workerID string, exits <-chan process.Exit) {
	select {
	case exit, ok := <-exits:
		if !ok {
			return
		}
		select {
		case coordinator.commands <- workerExitedCommand{workerID: workerID, exit: exit}:
		case <-coordinator.done:
		}
	case <-coordinator.done:
	}
}

func trackRequest(workers map[string]*worker, requests map[uint64]*request, command trackRequestCommand) error {
	if command.id == 0 {
		return errors.New("request ID must be greater than zero")
	}
	if _, exists := requests[command.id]; exists {
		return fmt.Errorf("request %d is already tracked", command.id)
	}
	if len(command.workerIDs) == 0 {
		return fmt.Errorf("request %d must include at least one worker", command.id)
	}

	results := make(map[string]protocol.Verdict, len(command.workerIDs))
	for _, workerID := range command.workerIDs {
		worker, exists := workers[workerID]
		if !exists {
			return fmt.Errorf("worker %q is not registered", workerID)
		}
		if _, duplicate := results[workerID]; duplicate {
			return fmt.Errorf("worker %q is duplicated for request %d", workerID, command.id)
		}
		if worker.state == WorkerFailed {
			results[workerID] = protocol.VerdictUnavailable
			continue
		}
		results[workerID] = ""
	}

	requests[command.id] = &request{id: command.id, results: results}
	return nil
}

func recordResult(requests map[uint64]*request, command recordResultCommand) error {
	request, exists := requests[command.requestID]
	if !exists {
		return fmt.Errorf("request %d is not tracked", command.requestID)
	}
	result, expected := request.results[command.workerID]
	if !expected {
		return fmt.Errorf("worker %q is not assigned to request %d", command.workerID, command.requestID)
	}
	if result != "" {
		return fmt.Errorf("worker %q already has a result for request %d", command.workerID, command.requestID)
	}
	if !validVerdict(command.verdict) {
		return fmt.Errorf("invalid verdict %q", command.verdict)
	}
	request.results[command.workerID] = command.verdict
	return nil
}

func validVerdict(verdict protocol.Verdict) bool {
	switch verdict {
	case protocol.VerdictClean,
		protocol.VerdictMalware,
		protocol.VerdictError,
		protocol.VerdictTimeout,
		protocol.VerdictUnavailable:
		return true
	default:
		return false
	}
}

func recordWorkerExit(workers map[string]*worker, requests map[uint64]*request, command workerExitedCommand) {
	worker, exists := workers[command.workerID]
	if !exists || worker.pid != command.exit.PID {
		return
	}
	worker.state = WorkerFailed
	worker.exitCode = command.exit.Code
	if command.exit.Err != nil {
		worker.exitError = command.exit.Err.Error()
	}

	for _, request := range requests {
		if result, expected := request.results[command.workerID]; expected && result == "" {
			request.results[command.workerID] = protocol.VerdictUnavailable
		}
	}
}

func makeSnapshot(workers map[string]*worker, requests map[uint64]*request) Snapshot {
	snapshot := Snapshot{
		Workers:  make(map[string]WorkerSnapshot, len(workers)),
		Requests: make(map[uint64]RequestSnapshot, len(requests)),
	}
	for id, worker := range workers {
		snapshot.Workers[id] = WorkerSnapshot{
			ID:        worker.id,
			PID:       worker.pid,
			State:     worker.state,
			ExitCode:  worker.exitCode,
			ExitError: worker.exitError,
		}
	}
	for id, request := range requests {
		results := make(map[string]protocol.Verdict, len(request.results))
		for workerID, verdict := range request.results {
			results[workerID] = verdict
		}
		snapshot.Requests[id] = RequestSnapshot{ID: request.id, Results: results}
	}
	return snapshot
}
