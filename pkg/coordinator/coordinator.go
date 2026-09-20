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
	WorkerHealthy  WorkerState = "HEALTHY"
	WorkerActive   WorkerState = "ACTIVE"
	WorkerDraining WorkerState = "DRAINING"
	WorkerRetired  WorkerState = "RETIRED"
	WorkerFailed   WorkerState = "FAILED"
)

type WorkerKey struct {
	ScannerID string
	Version   string
}

func (key WorkerKey) String() string {
	return key.ScannerID + "@" + key.Version
}

type WorkerSnapshot struct {
	Key           WorkerKey
	PID           int
	State         WorkerState
	FailureReason string
	ExitCode      int
	ExitError     string
}

type RequestSnapshot struct {
	ID      uint64
	Results map[WorkerKey]protocol.Verdict
}

type Snapshot struct {
	Workers  map[WorkerKey]WorkerSnapshot
	Active   map[string]WorkerKey
	Requests map[uint64]RequestSnapshot
}

type Coordinator struct {
	commands chan command
	done     chan struct{}
}

type worker struct {
	key           WorkerKey
	child         *process.Child
	pid           int
	state         WorkerState
	failureReason string
	exitCode      int
	exitError     string
}

type request struct {
	id      uint64
	results map[WorkerKey]protocol.Verdict
}

type state struct {
	workers  map[WorkerKey]*worker
	active   map[string]WorkerKey
	requests map[uint64]*request
}

type command interface {
	isCommand()
}

type registerWorkerCommand struct {
	key   WorkerKey
	child *process.Child
	reply chan error
}

func (registerWorkerCommand) isCommand() {}

type markHealthyCommand struct {
	key   WorkerKey
	reply chan error
}

func (markHealthyCommand) isCommand() {}

type failWorkerCommand struct {
	key    WorkerKey
	reason string
	reply  chan error
}

func (failWorkerCommand) isCommand() {}

type activateWorkerCommand struct {
	key   WorkerKey
	reply chan error
}

func (activateWorkerCommand) isCommand() {}

type retireWorkerCommand struct {
	key   WorkerKey
	reply chan error
}

func (retireWorkerCommand) isCommand() {}

type trackRequestCommand struct {
	id         uint64
	workerKeys []WorkerKey
	reply      chan error
}

func (trackRequestCommand) isCommand() {}

type recordResultCommand struct {
	requestID uint64
	workerKey WorkerKey
	verdict   protocol.Verdict
	reply     chan error
}

func (recordResultCommand) isCommand() {}

type snapshotCommand struct {
	reply chan snapshotResult
}

func (snapshotCommand) isCommand() {}

type workerExitedCommand struct {
	workerKey WorkerKey
	exit      process.Exit
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

func (coordinator *Coordinator) RegisterWorker(ctx context.Context, key WorkerKey, child *process.Child) error {
	reply := make(chan error, 1)
	if err := coordinator.send(ctx, registerWorkerCommand{key: key, child: child, reply: reply}); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) MarkHealthy(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := coordinator.send(ctx, markHealthyCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) FailWorker(ctx context.Context, key WorkerKey, reason string) error {
	reply := make(chan error, 1)
	if err := coordinator.send(ctx, failWorkerCommand{key: key, reason: reason, reply: reply}); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) ActivateWorker(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := coordinator.send(ctx, activateWorkerCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) RetireWorker(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := coordinator.send(ctx, retireWorkerCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) TrackRequest(ctx context.Context, id uint64, workerKeys []WorkerKey) error {
	reply := make(chan error, 1)
	command := trackRequestCommand{id: id, workerKeys: append([]WorkerKey(nil), workerKeys...), reply: reply}
	if err := coordinator.send(ctx, command); err != nil {
		return err
	}
	return coordinator.waitError(ctx, reply)
}

func (coordinator *Coordinator) RecordResult(ctx context.Context, requestID uint64, workerKey WorkerKey, verdict protocol.Verdict) error {
	reply := make(chan error, 1)
	command := recordResultCommand{
		requestID: requestID,
		workerKey: workerKey,
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
	current := state{
		workers:  make(map[WorkerKey]*worker),
		active:   make(map[string]WorkerKey),
		requests: make(map[uint64]*request),
	}

	for {
		select {
		case <-ctx.Done():
			return
		case received := <-coordinator.commands:
			switch command := received.(type) {
			case registerWorkerCommand:
				command.reply <- coordinator.registerWorker(&current, command)
			case markHealthyCommand:
				command.reply <- markHealthy(&current, command)
			case failWorkerCommand:
				command.reply <- failWorker(&current, command)
			case activateWorkerCommand:
				command.reply <- activateWorker(&current, command)
			case retireWorkerCommand:
				command.reply <- retireWorker(&current, command)
			case trackRequestCommand:
				command.reply <- trackRequest(&current, command)
			case recordResultCommand:
				command.reply <- recordResult(&current, command)
			case snapshotCommand:
				command.reply <- snapshotResult{snapshot: makeSnapshot(current)}
			case workerExitedCommand:
				recordWorkerExit(&current, command)
			}
		}
	}
}

func (coordinator *Coordinator) registerWorker(current *state, command registerWorkerCommand) error {
	if err := validateWorkerKey(command.key); err != nil {
		return err
	}
	if command.child == nil {
		return errors.New("worker child must not be nil")
	}
	if _, exists := current.workers[command.key]; exists {
		return fmt.Errorf("worker %q is already registered", command.key)
	}
	for _, registered := range current.workers {
		if registered.key.ScannerID == command.key.ScannerID && (registered.state == WorkerStarting || registered.state == WorkerHealthy) {
			return fmt.Errorf("scanner %q already has candidate %q", command.key.ScannerID, registered.key)
		}
	}

	current.workers[command.key] = &worker{
		key:   command.key,
		child: command.child,
		pid:   command.child.PID(),
		state: WorkerStarting,
	}
	go coordinator.monitor(command.key, command.child.Exited())
	return nil
}

func (coordinator *Coordinator) monitor(workerKey WorkerKey, exits <-chan process.Exit) {
	select {
	case exit, ok := <-exits:
		if !ok {
			return
		}
		select {
		case coordinator.commands <- workerExitedCommand{workerKey: workerKey, exit: exit}:
		case <-coordinator.done:
		}
	case <-coordinator.done:
	}
}

func markHealthy(current *state, command markHealthyCommand) error {
	worker, err := findWorker(current, command.key)
	if err != nil {
		return err
	}
	if worker.state != WorkerStarting {
		return invalidTransition(command.key, worker.state, WorkerHealthy)
	}
	worker.state = WorkerHealthy
	return nil
}

func failWorker(current *state, command failWorkerCommand) error {
	worker, err := findWorker(current, command.key)
	if err != nil {
		return err
	}
	if worker.state != WorkerStarting && worker.state != WorkerHealthy {
		return fmt.Errorf("worker %q cannot fail before activation from state %q", command.key, worker.state)
	}
	if strings.TrimSpace(command.reason) == "" {
		return errors.New("worker failure reason must not be empty")
	}
	worker.state = WorkerFailed
	worker.failureReason = command.reason
	markPendingUnavailable(current.requests, command.key)
	if err := worker.child.Terminate(); err != nil {
		return fmt.Errorf("terminate failed worker %q: %w", command.key, err)
	}
	return nil
}

func activateWorker(current *state, command activateWorkerCommand) error {
	worker, err := findWorker(current, command.key)
	if err != nil {
		return err
	}
	if worker.state != WorkerHealthy {
		return invalidTransition(command.key, worker.state, WorkerActive)
	}
	if activeKey, exists := current.active[command.key.ScannerID]; exists {
		active := current.workers[activeKey]
		if active == nil || active.state != WorkerActive {
			return fmt.Errorf("scanner %q has invalid active route %q", command.key.ScannerID, activeKey)
		}
		active.state = WorkerDraining
	}
	worker.state = WorkerActive
	current.active[command.key.ScannerID] = command.key
	return nil
}

func retireWorker(current *state, command retireWorkerCommand) error {
	worker, err := findWorker(current, command.key)
	if err != nil {
		return err
	}
	if worker.state != WorkerDraining {
		return invalidTransition(command.key, worker.state, WorkerRetired)
	}
	for requestID, request := range current.requests {
		if result, assigned := request.results[command.key]; assigned && result == "" {
			return fmt.Errorf("worker %q still has in-flight request %d", command.key, requestID)
		}
	}
	worker.state = WorkerRetired
	return nil
}

func trackRequest(current *state, command trackRequestCommand) error {
	if command.id == 0 {
		return errors.New("request ID must be greater than zero")
	}
	if _, exists := current.requests[command.id]; exists {
		return fmt.Errorf("request %d is already tracked", command.id)
	}
	if len(command.workerKeys) == 0 {
		return fmt.Errorf("request %d must include at least one worker", command.id)
	}

	results := make(map[WorkerKey]protocol.Verdict, len(command.workerKeys))
	for _, workerKey := range command.workerKeys {
		worker, exists := current.workers[workerKey]
		if !exists {
			return fmt.Errorf("worker %q is not registered", workerKey)
		}
		if _, duplicate := results[workerKey]; duplicate {
			return fmt.Errorf("worker %q is duplicated for request %d", workerKey, command.id)
		}
		switch worker.state {
		case WorkerActive:
			results[workerKey] = ""
		case WorkerFailed:
			results[workerKey] = protocol.VerdictUnavailable
		default:
			return fmt.Errorf("worker %q in state %q cannot receive a request", workerKey, worker.state)
		}
	}

	current.requests[command.id] = &request{id: command.id, results: results}
	return nil
}

func recordResult(current *state, command recordResultCommand) error {
	request, exists := current.requests[command.requestID]
	if !exists {
		return fmt.Errorf("request %d is not tracked", command.requestID)
	}
	result, expected := request.results[command.workerKey]
	if !expected {
		return fmt.Errorf("worker %q is not assigned to request %d", command.workerKey, command.requestID)
	}
	if result != "" {
		return fmt.Errorf("worker %q already has a result for request %d", command.workerKey, command.requestID)
	}
	if !validVerdict(command.verdict) {
		return fmt.Errorf("invalid verdict %q", command.verdict)
	}
	request.results[command.workerKey] = command.verdict
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

func recordWorkerExit(current *state, command workerExitedCommand) {
	worker, exists := current.workers[command.workerKey]
	if !exists || worker.pid != command.exit.PID {
		return
	}
	worker.exitCode = command.exit.Code
	if command.exit.Err != nil {
		worker.exitError = command.exit.Err.Error()
	}
	if worker.state == WorkerRetired {
		return
	}
	worker.state = WorkerFailed
	if worker.failureReason == "" {
		worker.failureReason = "worker exited unexpectedly"
	}
	if activeKey, active := current.active[command.workerKey.ScannerID]; active && activeKey == command.workerKey {
		delete(current.active, command.workerKey.ScannerID)
	}
	markPendingUnavailable(current.requests, command.workerKey)
}

func markPendingUnavailable(requests map[uint64]*request, workerKey WorkerKey) {
	for _, request := range requests {
		if result, expected := request.results[workerKey]; expected && result == "" {
			request.results[workerKey] = protocol.VerdictUnavailable
		}
	}
}

func validateWorkerKey(key WorkerKey) error {
	if strings.TrimSpace(key.ScannerID) == "" {
		return errors.New("worker scanner ID must not be empty")
	}
	if strings.TrimSpace(key.Version) == "" {
		return errors.New("worker version must not be empty")
	}
	return nil
}

func findWorker(current *state, key WorkerKey) (*worker, error) {
	worker, exists := current.workers[key]
	if !exists {
		return nil, fmt.Errorf("worker %q is not registered", key)
	}
	return worker, nil
}

func invalidTransition(key WorkerKey, from WorkerState, to WorkerState) error {
	return fmt.Errorf("worker %q cannot transition from %q to %q", key, from, to)
}

func makeSnapshot(current state) Snapshot {
	snapshot := Snapshot{
		Workers:  make(map[WorkerKey]WorkerSnapshot, len(current.workers)),
		Active:   make(map[string]WorkerKey, len(current.active)),
		Requests: make(map[uint64]RequestSnapshot, len(current.requests)),
	}
	for key, worker := range current.workers {
		snapshot.Workers[key] = WorkerSnapshot{
			Key:           worker.key,
			PID:           worker.pid,
			State:         worker.state,
			FailureReason: worker.failureReason,
			ExitCode:      worker.exitCode,
			ExitError:     worker.exitError,
		}
	}
	for scannerID, key := range current.active {
		snapshot.Active[scannerID] = key
	}
	for id, request := range current.requests {
		results := make(map[WorkerKey]protocol.Verdict, len(request.results))
		for workerKey, verdict := range request.results {
			results[workerKey] = verdict
		}
		snapshot.Requests[id] = RequestSnapshot{ID: request.id, Results: results}
	}
	return snapshot
}
