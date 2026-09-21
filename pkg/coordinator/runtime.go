package coordinator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"miniav/pkg/aggregator"
	"miniav/pkg/config"
	processmanager "miniav/pkg/process"
	"miniav/pkg/protocol"
	"miniav/pkg/workertransport"
)

type ScanResult struct {
	RequestID uint64
	Verdict   aggregator.Verdict
	Results   map[WorkerKey]protocol.Message
}

type ReloadResult struct {
	ScannerID      string
	WorkerVersion  string
	SignatureCount int
}

type RuntimeSnapshot struct {
	Workers []WorkerSnapshot
}

type Runtime struct {
	events         chan any
	done           chan struct{}
	cancel         context.CancelFunc
	startupTimeout time.Duration
	scanTimeout    time.Duration
	stderr         io.Writer
	logger         *slog.Logger
}

const postAckExitGrace = 250 * time.Millisecond

type runtimeWorker struct {
	key                  WorkerKey
	child                *processmanager.Child
	connection           workertransport.Connection
	transport            string
	outbound             chan protocol.Message
	state                WorkerState
	signaturePath        string
	failureReason        string
	exitCode             int
	exitError            string
	inFlight             int
	helloReply           chan error
	healthReply          chan error
	reloadReply          chan reloadOutcome
	reloadTimer          *time.Timer
	pendingSignaturePath string
	shutdownSent         bool
	shutdownAck          bool
	exited               bool
}

type runtimeRequest struct {
	id      uint64
	path    string
	results map[WorkerKey]protocol.Message
	reply   chan scanOutcome
	timer   *time.Timer
}

type runtimeState struct {
	workers       map[WorkerKey]*runtimeWorker
	active        map[string]WorkerKey
	requests      map[uint64]*runtimeRequest
	nextRequestID uint64
	stopReply     chan error
}

type registerRuntimeCommand struct {
	worker     config.Worker
	child      *processmanager.Child
	connection workertransport.Connection
	reply      chan error
}

type workerMessageEvent struct {
	key     WorkerKey
	message protocol.Message
}

type workerFailureEvent struct {
	key    WorkerKey
	reason string
}

type workerExitEvent struct {
	key  WorkerKey
	exit processmanager.Exit
}

type helloRuntimeCommand struct {
	key   WorkerKey
	reply chan error
}

type healthRuntimeCommand struct {
	key   WorkerKey
	reply chan error
}

type activateRuntimeCommand struct {
	key   WorkerKey
	reply chan error
}

type failRuntimeCommand struct {
	key    WorkerKey
	reason string
	reply  chan error
}

type scanRuntimeCommand struct {
	path  string
	reply chan scanOutcome
}

type scanTimeoutEvent struct {
	id uint64
}

type scanOutcome struct {
	result ScanResult
	err    error
}

type reloadRuntimeCommand struct {
	scannerID string
	path      string
	reply     chan reloadOutcome
}

type reloadTimeoutEvent struct {
	key WorkerKey
}

type reloadOutcome struct {
	result ReloadResult
	err    error
}

type snapshotRuntimeCommand struct {
	reply chan RuntimeSnapshot
}

type activeWorkerCommand struct {
	scannerID string
	reply     chan activeWorkerResult
}

type activeWorkerResult struct {
	worker config.Worker
	err    error
}

type shutdownRuntimeCommand struct {
	reply chan error
}

func NewRuntime(parent context.Context, startupTimeout time.Duration, scanTimeout time.Duration, stderr io.Writer) (*Runtime, error) {
	if startupTimeout <= 0 || scanTimeout <= 0 {
		return nil, errors.New("runtime timeouts must be greater than zero")
	}
	if stderr == nil {
		stderr = io.Discard
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &Runtime{
		events:         make(chan any, 128),
		done:           make(chan struct{}),
		cancel:         cancel,
		startupTimeout: startupTimeout,
		scanTimeout:    scanTimeout,
		stderr:         stderr,
		logger:         slog.New(slog.NewTextHandler(stderr, nil)),
	}
	go runtime.run(ctx)
	return runtime, nil
}

func (runtime *Runtime) StartWorker(ctx context.Context, worker config.Worker) error {
	args := []string{"-worker-version", worker.WorkerVersion, "-signatures", worker.SignaturePath}
	if worker.Transport != workertransport.Stdio {
		args = append(args, "-listen", "127.0.0.1:0")
	}
	if worker.DelayMs != 0 {
		args = append(args, "-delay", fmt.Sprint(worker.DelayMs))
	}
	if worker.CrashOnScan {
		args = append(args, "-crash-on-scan")
	}
	if worker.FailHealth {
		args = append(args, "-fail-health")
	}
	child, err := processmanager.Start(worker.Executable, args, runtime.stderr)
	if err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(ctx, runtime.startupTimeout)
	defer cancel()
	connection, err := runtime.connectWorker(startupCtx, worker.Transport, child)
	if err != nil {
		child.Terminate()
		return err
	}
	key := WorkerKey{ScannerID: worker.ScannerID, Version: worker.WorkerVersion}
	registerReply := make(chan error, 1)
	if err := runtime.send(ctx, registerRuntimeCommand{worker: worker, child: child, connection: connection, reply: registerReply}); err != nil {
		connection.Close()
		child.Terminate()
		return err
	}
	if err := waitRuntimeReply(ctx, runtime.done, registerReply); err != nil {
		connection.Close()
		child.Terminate()
		return err
	}
	if err := runtime.hello(startupCtx, key); err != nil {
		runtime.failCandidate(key, err)
		return fmt.Errorf("worker %q handshake: %w", key, err)
	}
	if err := runtime.health(startupCtx, key); err != nil {
		runtime.failCandidate(key, err)
		return fmt.Errorf("worker %q health check: %w", key, err)
	}
	if err := runtime.activate(startupCtx, key); err != nil {
		runtime.failCandidate(key, err)
		return err
	}
	return nil
}

func (runtime *Runtime) connectWorker(ctx context.Context, mode string, child *processmanager.Child) (workertransport.Connection, error) {
	if mode == workertransport.Stdio {
		return workertransport.NewStream(child.Stdout(), child.Stdin(), child.Stdout(), child.Stdin()), nil
	}
	type result struct {
		bootstrap workertransport.Bootstrap
		err       error
	}
	ready := make(chan result, 1)
	go func() {
		bootstrap, err := workertransport.ReadBootstrap(child.Stdout())
		ready <- result{bootstrap: bootstrap, err: err}
	}()
	select {
	case received := <-ready:
		if received.err != nil {
			return nil, received.err
		}
		return workertransport.Dial(ctx, mode, received.bootstrap.Address)
	case exit := <-child.Exited():
		return nil, fmt.Errorf("worker exited during %s bootstrap with code %d", mode, exit.Code)
	case <-ctx.Done():
		return nil, fmt.Errorf("worker %s bootstrap: %w", mode, ctx.Err())
	}
}

func (runtime *Runtime) Scan(ctx context.Context, path string) (ScanResult, error) {
	absolute, err := filepathRegular(path)
	if err != nil {
		return ScanResult{}, err
	}
	reply := make(chan scanOutcome, 1)
	if err := runtime.send(ctx, scanRuntimeCommand{path: absolute, reply: reply}); err != nil {
		return ScanResult{}, err
	}
	select {
	case outcome := <-reply:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return ScanResult{}, ctx.Err()
	case <-runtime.done:
		return ScanResult{}, ErrStopped
	}
}

func (runtime *Runtime) Reload(ctx context.Context, scannerID string, path string) (ReloadResult, error) {
	absolute, err := filepathRegular(path)
	if err != nil {
		return ReloadResult{}, err
	}
	reply := make(chan reloadOutcome, 1)
	if err := runtime.send(ctx, reloadRuntimeCommand{scannerID: scannerID, path: absolute, reply: reply}); err != nil {
		return ReloadResult{}, err
	}
	select {
	case outcome := <-reply:
		return outcome.result, outcome.err
	case <-ctx.Done():
		return ReloadResult{}, ctx.Err()
	case <-runtime.done:
		return ReloadResult{}, ErrStopped
	}
}

func (runtime *Runtime) ActiveWorker(ctx context.Context, scannerID string) (config.Worker, error) {
	reply := make(chan activeWorkerResult, 1)
	if err := runtime.send(ctx, activeWorkerCommand{scannerID: scannerID, reply: reply}); err != nil {
		return config.Worker{}, err
	}
	select {
	case result := <-reply:
		return result.worker, result.err
	case <-ctx.Done():
		return config.Worker{}, ctx.Err()
	case <-runtime.done:
		return config.Worker{}, ErrStopped
	}
}

func (runtime *Runtime) Snapshot(ctx context.Context) (RuntimeSnapshot, error) {
	reply := make(chan RuntimeSnapshot, 1)
	if err := runtime.send(ctx, snapshotRuntimeCommand{reply: reply}); err != nil {
		return RuntimeSnapshot{}, err
	}
	select {
	case snapshot := <-reply:
		return snapshot, nil
	case <-ctx.Done():
		return RuntimeSnapshot{}, ctx.Err()
	case <-runtime.done:
		return RuntimeSnapshot{}, ErrStopped
	}
}

func (runtime *Runtime) Shutdown(ctx context.Context) error {
	reply := make(chan error, 1)
	if err := runtime.send(ctx, shutdownRuntimeCommand{reply: reply}); err != nil {
		return err
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		runtime.cancel()
		<-runtime.done
		return ctx.Err()
	case <-runtime.done:
		return nil
	}
}

func (runtime *Runtime) Done() <-chan struct{} {
	return runtime.done
}

func (runtime *Runtime) hello(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := runtime.send(ctx, helloRuntimeCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return waitRuntimeReply(ctx, runtime.done, reply)
}

func (runtime *Runtime) health(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := runtime.send(ctx, healthRuntimeCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return waitRuntimeReply(ctx, runtime.done, reply)
}

func (runtime *Runtime) activate(ctx context.Context, key WorkerKey) error {
	reply := make(chan error, 1)
	if err := runtime.send(ctx, activateRuntimeCommand{key: key, reply: reply}); err != nil {
		return err
	}
	return waitRuntimeReply(ctx, runtime.done, reply)
}

func (runtime *Runtime) failCandidate(key WorkerKey, failure error) {
	ctx, cancel := context.WithTimeout(context.Background(), runtime.startupTimeout)
	defer cancel()
	reply := make(chan error, 1)
	if runtime.send(ctx, failRuntimeCommand{key: key, reason: failure.Error(), reply: reply}) == nil {
		waitRuntimeReply(ctx, runtime.done, reply)
	}
}

func (runtime *Runtime) send(ctx context.Context, event any) error {
	select {
	case runtime.events <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-runtime.done:
		return ErrStopped
	}
}

func (runtime *Runtime) emit(event any) {
	select {
	case runtime.events <- event:
	case <-runtime.done:
	}
}

func (runtime *Runtime) run(ctx context.Context) {
	state := runtimeState{
		workers:  make(map[WorkerKey]*runtimeWorker),
		active:   make(map[string]WorkerKey),
		requests: make(map[uint64]*runtimeRequest),
	}
	defer func() {
		for _, worker := range state.workers {
			close(worker.outbound)
			worker.connection.Close()
			if worker.state != WorkerRetired {
				worker.child.Terminate()
			}
		}
		close(runtime.done)
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case received := <-runtime.events:
			switch event := received.(type) {
			case registerRuntimeCommand:
				event.reply <- runtime.registerRuntimeWorker(&state, event)
			case helloRuntimeCommand:
				runtime.beginHello(&state, event)
			case healthRuntimeCommand:
				runtime.beginHealth(&state, event)
			case activateRuntimeCommand:
				event.reply <- runtime.activateRuntimeWorker(&state, event.key)
			case failRuntimeCommand:
				event.reply <- runtime.failRuntimeWorker(&state, event.key, event.reason)
			case workerMessageEvent:
				runtime.handleWorkerMessage(&state, event)
			case workerFailureEvent:
				runtime.handleWorkerFailure(&state, event.key, event.reason)
			case workerExitEvent:
				runtime.handleWorkerExit(&state, event)
			case scanRuntimeCommand:
				runtime.beginScan(&state, event)
			case scanTimeoutEvent:
				runtime.timeoutScan(&state, event.id)
			case reloadRuntimeCommand:
				runtime.beginReload(&state, event)
			case reloadTimeoutEvent:
				runtime.timeoutReload(&state, event.key)
			case snapshotRuntimeCommand:
				event.reply <- runtime.runtimeSnapshot(state)
			case activeWorkerCommand:
				event.reply <- runtime.activeWorker(state, event.scannerID)
			case shutdownRuntimeCommand:
				if state.stopReply != nil {
					event.reply <- errors.New("shutdown is already in progress")
					continue
				}
				state.stopReply = event.reply
				runtime.beginShutdown(&state)
			}
			if state.stopReply != nil && runtime.allWorkersStopped(state) {
				state.stopReply <- nil
				return
			}
		}
	}
}

func (runtime *Runtime) registerRuntimeWorker(state *runtimeState, command registerRuntimeCommand) error {
	key := WorkerKey{ScannerID: command.worker.ScannerID, Version: command.worker.WorkerVersion}
	if _, exists := state.workers[key]; exists {
		return fmt.Errorf("worker %q is already registered", key)
	}
	for _, worker := range state.workers {
		if worker.key.ScannerID == key.ScannerID && (worker.state == WorkerStarting || worker.state == WorkerHealthy) {
			return fmt.Errorf("scanner %q already has candidate %q", key.ScannerID, worker.key)
		}
	}
	worker := &runtimeWorker{key: key, child: command.child, connection: command.connection, transport: command.worker.Transport, outbound: make(chan protocol.Message, 64), state: WorkerStarting, signaturePath: command.worker.SignaturePath}
	state.workers[key] = worker
	runtime.logger.Info("worker registered", "worker", key.String(), "pid", command.child.PID())
	go runtime.writeWorker(worker)
	go runtime.readWorker(worker)
	go func() {
		exit, ok := <-worker.child.Exited()
		if ok {
			runtime.emit(workerExitEvent{key: key, exit: exit})
		}
	}()
	return nil
}

func (runtime *Runtime) writeWorker(worker *runtimeWorker) {
	for message := range worker.outbound {
		if err := worker.connection.Send(message); err != nil {
			runtime.emit(workerFailureEvent{key: worker.key, reason: fmt.Sprintf("write worker protocol: %v", err)})
			return
		}
	}
}

func (runtime *Runtime) readWorker(worker *runtimeWorker) {
	for {
		message, err := worker.connection.Receive()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				runtime.emit(workerFailureEvent{key: worker.key, reason: fmt.Sprintf("read worker protocol: %v", err)})
			}
			return
		}
		runtime.emit(workerMessageEvent{key: worker.key, message: message})
	}
}

func (runtime *Runtime) beginHello(state *runtimeState, command helloRuntimeCommand) {
	worker := state.workers[command.key]
	if worker == nil || worker.state != WorkerStarting {
		command.reply <- fmt.Errorf("worker %q is not starting", command.key)
		return
	}
	if worker.helloReply != nil {
		command.reply <- fmt.Errorf("worker %q handshake is already pending", command.key)
		return
	}
	worker.helloReply = command.reply
	if err := queueWorkerMessage(worker, protocol.Message{Version: protocol.Version, Type: protocol.TypeHello}); err != nil {
		worker.helloReply = nil
		command.reply <- err
		runtime.handleWorkerFailure(state, command.key, err.Error())
	}
}

func (runtime *Runtime) beginHealth(state *runtimeState, command healthRuntimeCommand) {
	worker := state.workers[command.key]
	if worker == nil || worker.state != WorkerStarting {
		command.reply <- fmt.Errorf("worker %q is not starting", command.key)
		return
	}
	if worker.healthReply != nil {
		command.reply <- fmt.Errorf("worker %q health check is already pending", command.key)
		return
	}
	worker.healthReply = command.reply
	if err := queueWorkerMessage(worker, protocol.Message{Version: protocol.Version, Type: protocol.TypeHealthCheck}); err != nil {
		worker.healthReply = nil
		command.reply <- err
		runtime.handleWorkerFailure(state, command.key, err.Error())
	}
}

func (runtime *Runtime) activateRuntimeWorker(state *runtimeState, key WorkerKey) error {
	worker := state.workers[key]
	if worker == nil || worker.state != WorkerHealthy {
		return fmt.Errorf("worker %q is not healthy", key)
	}
	if previousKey, exists := state.active[key.ScannerID]; exists {
		previous := state.workers[previousKey]
		previous.state = WorkerDraining
		if previous.inFlight == 0 {
			runtime.requestWorkerShutdown(previous)
		}
	}
	worker.state = WorkerActive
	state.active[key.ScannerID] = key
	runtime.logger.Info("worker activated", "worker", key.String(), "pid", worker.child.PID())
	return nil
}

func (runtime *Runtime) failRuntimeWorker(state *runtimeState, key WorkerKey, reason string) error {
	worker := state.workers[key]
	if worker == nil {
		return fmt.Errorf("worker %q is not registered", key)
	}
	if worker.state != WorkerStarting && worker.state != WorkerHealthy {
		return fmt.Errorf("worker %q cannot fail from state %q", key, worker.state)
	}
	runtime.handleWorkerFailure(state, key, reason)
	worker.child.Terminate()
	return nil
}

func (runtime *Runtime) handleWorkerMessage(state *runtimeState, event workerMessageEvent) {
	worker := state.workers[event.key]
	if worker == nil || worker.state == WorkerFailed || worker.state == WorkerRetired {
		return
	}
	switch event.message.Type {
	case protocol.TypeHelloAck:
		if worker.helloReply == nil {
			runtime.handleWorkerFailure(state, event.key, "unexpected HELLO_ACK")
			return
		}
		err := error(nil)
		if event.message.ScannerID != worker.key.ScannerID || event.message.WorkerVersion != worker.key.Version {
			err = fmt.Errorf("identity mismatch: got %s@%s", event.message.ScannerID, event.message.WorkerVersion)
		}
		worker.helloReply <- err
		worker.helloReply = nil
	case protocol.TypeHealthResult:
		if worker.healthReply == nil {
			runtime.handleWorkerFailure(state, event.key, "unexpected HEALTH_RESULT")
			return
		}
		var err error
		if event.message.Status != "ok" {
			err = fmt.Errorf("worker reported unhealthy: %s", event.message.Error)
		} else {
			worker.state = WorkerHealthy
		}
		worker.healthReply <- err
		worker.healthReply = nil
	case protocol.TypeScanResult:
		runtime.recordRuntimeScanResult(state, worker, event.message)
	case protocol.TypeReloadResult:
		runtime.recordReloadResult(worker, event.message)
	case protocol.TypeShutdownAck:
		if worker.state != WorkerDraining {
			runtime.handleWorkerFailure(state, event.key, "unexpected SHUTDOWN_ACK")
			return
		}
		worker.shutdownAck = true
		time.AfterFunc(postAckExitGrace, func() { worker.child.Terminate() })
	default:
		runtime.handleWorkerFailure(state, event.key, fmt.Sprintf("unexpected worker message %q", event.message.Type))
	}
}

func (runtime *Runtime) beginScan(state *runtimeState, command scanRuntimeCommand) {
	if state.stopReply != nil {
		command.reply <- scanOutcome{err: errors.New("core is shutting down")}
		return
	}
	if len(state.active) == 0 {
		command.reply <- scanOutcome{err: errors.New("no active workers")}
		return
	}
	state.nextRequestID++
	request := &runtimeRequest{id: state.nextRequestID, path: command.path, results: make(map[WorkerKey]protocol.Message, len(state.active)), reply: command.reply}
	state.requests[request.id] = request
	keys := make([]WorkerKey, 0, len(state.active))
	for _, key := range state.active {
		worker := state.workers[key]
		keys = append(keys, key)
		request.results[key] = protocol.Message{}
		worker.inFlight++
	}
	request.timer = time.AfterFunc(runtime.scanTimeout, func() { runtime.emit(scanTimeoutEvent{id: request.id}) })
	for _, key := range keys {
		worker := state.workers[key]
		if err := queueWorkerMessage(worker, protocol.Message{Version: protocol.Version, Type: protocol.TypeScan, RequestID: request.id, FilePath: command.path}); err != nil {
			runtime.handleWorkerFailure(state, key, err.Error())
		}
	}
}

func (runtime *Runtime) recordRuntimeScanResult(state *runtimeState, worker *runtimeWorker, message protocol.Message) {
	request := state.requests[message.RequestID]
	if request == nil {
		return
	}
	current, expected := request.results[worker.key]
	if !expected || current.Type != "" {
		return
	}
	request.results[worker.key] = message
	runtime.completeRequestIfReady(state, request)
}

func (runtime *Runtime) timeoutScan(state *runtimeState, id uint64) {
	request := state.requests[id]
	if request == nil {
		return
	}
	for key, result := range request.results {
		if result.Type == "" {
			request.results[key] = protocol.Message{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: id, Verdict: protocol.VerdictTimeout, Error: "scan timed out"}
		}
	}
	runtime.completeRequest(state, request)
}

func (runtime *Runtime) completeRequestIfReady(state *runtimeState, request *runtimeRequest) {
	for _, result := range request.results {
		if result.Type == "" {
			return
		}
	}
	runtime.completeRequest(state, request)
}

func (runtime *Runtime) completeRequest(state *runtimeState, request *runtimeRequest) {
	if request.timer != nil {
		request.timer.Stop()
	}
	verdicts := make([]protocol.Verdict, 0, len(request.results))
	for key, result := range request.results {
		verdicts = append(verdicts, result.Verdict)
		worker := state.workers[key]
		if worker != nil && worker.inFlight > 0 {
			worker.inFlight--
			if worker.state == WorkerDraining && worker.inFlight == 0 {
				runtime.requestWorkerShutdown(worker)
			}
		}
	}
	verdict, err := aggregator.Aggregate(verdicts)
	request.reply <- scanOutcome{result: ScanResult{RequestID: request.id, Verdict: verdict, Results: request.results}, err: err}
	delete(state.requests, request.id)
}

func (runtime *Runtime) beginReload(state *runtimeState, command reloadRuntimeCommand) {
	key, exists := state.active[command.scannerID]
	if !exists {
		command.reply <- reloadOutcome{err: fmt.Errorf("scanner %q has no active worker", command.scannerID)}
		return
	}
	worker := state.workers[key]
	if worker.reloadReply != nil {
		command.reply <- reloadOutcome{err: fmt.Errorf("scanner %q already has a reload in progress", command.scannerID)}
		return
	}
	worker.reloadReply = command.reply
	worker.pendingSignaturePath = command.path
	if err := queueWorkerMessage(worker, protocol.Message{Version: protocol.Version, Type: protocol.TypeReload, SignaturePath: command.path}); err != nil {
		worker.reloadReply = nil
		worker.pendingSignaturePath = ""
		command.reply <- reloadOutcome{err: err}
		runtime.handleWorkerFailure(state, key, err.Error())
		return
	}
	worker.reloadTimer = time.AfterFunc(runtime.startupTimeout, func() { runtime.emit(reloadTimeoutEvent{key: key}) })
}

func (runtime *Runtime) recordReloadResult(worker *runtimeWorker, message protocol.Message) {
	if worker.reloadReply == nil {
		return
	}
	if worker.reloadTimer != nil {
		worker.reloadTimer.Stop()
	}
	outcome := reloadOutcome{result: ReloadResult{ScannerID: worker.key.ScannerID, WorkerVersion: worker.key.Version, SignatureCount: message.SignatureCount}}
	if !message.Success {
		outcome.err = errors.New(message.Error)
	} else {
		worker.signaturePath = worker.pendingSignaturePath
	}
	worker.pendingSignaturePath = ""
	worker.reloadReply <- outcome
	worker.reloadReply = nil
}

func (runtime *Runtime) timeoutReload(state *runtimeState, key WorkerKey) {
	worker := state.workers[key]
	if worker != nil && worker.reloadReply != nil {
		worker.reloadReply <- reloadOutcome{err: errors.New("signature reload timed out")}
		worker.reloadReply = nil
		worker.pendingSignaturePath = ""
	}
}

func (runtime *Runtime) handleWorkerFailure(state *runtimeState, key WorkerKey, reason string) {
	worker := state.workers[key]
	if worker == nil || worker.state == WorkerFailed || worker.state == WorkerRetired {
		return
	}
	worker.state = WorkerFailed
	worker.failureReason = reason
	runtime.logger.Error("worker failed", "worker", key.String(), "reason", reason)
	time.AfterFunc(0, func() { worker.child.Terminate() })
	if active, exists := state.active[key.ScannerID]; exists && active == key {
		delete(state.active, key.ScannerID)
	}
	if worker.helloReply != nil {
		worker.helloReply <- errors.New(reason)
		worker.helloReply = nil
	}
	if worker.healthReply != nil {
		worker.healthReply <- errors.New(reason)
		worker.healthReply = nil
	}
	if worker.reloadReply != nil {
		worker.reloadReply <- reloadOutcome{err: errors.New(reason)}
		worker.reloadReply = nil
	}
	for _, request := range state.requests {
		if result, expected := request.results[key]; expected && result.Type == "" {
			request.results[key] = protocol.Message{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: request.id, Verdict: protocol.VerdictUnavailable, Error: reason}
			runtime.completeRequestIfReady(state, request)
		}
	}
}

func (runtime *Runtime) handleWorkerExit(state *runtimeState, event workerExitEvent) {
	worker := state.workers[event.key]
	if worker == nil {
		return
	}
	worker.exitCode = event.exit.Code
	worker.exited = true
	if event.exit.Err != nil {
		worker.exitError = event.exit.Err.Error()
	}
	if worker.state == WorkerDraining && worker.shutdownAck {
		worker.state = WorkerRetired
		runtime.logger.Info("worker retired", "worker", event.key.String(), "pid", worker.child.PID())
	} else if worker.state != WorkerRetired {
		runtime.handleWorkerFailure(state, event.key, "worker exited unexpectedly")
	}
}

func (runtime *Runtime) requestWorkerShutdown(worker *runtimeWorker) {
	if worker.shutdownSent {
		return
	}
	worker.shutdownSent = true
	if err := queueWorkerMessage(worker, protocol.Message{Version: protocol.Version, Type: protocol.TypeShutdown}); err != nil {
		time.AfterFunc(0, func() { worker.child.Terminate() })
	}
}

func queueWorkerMessage(worker *runtimeWorker, message protocol.Message) error {
	select {
	case worker.outbound <- message:
		return nil
	default:
		return fmt.Errorf("worker %q outbound queue is full", worker.key)
	}
}

func (runtime *Runtime) beginShutdown(state *runtimeState) {
	for id, request := range state.requests {
		for key, result := range request.results {
			if result.Type == "" {
				request.results[key] = protocol.Message{Version: protocol.Version, Type: protocol.TypeScanResult, RequestID: id, Verdict: protocol.VerdictUnavailable, Error: "core is shutting down"}
			}
		}
		runtime.completeRequest(state, request)
	}
	for scannerID := range state.active {
		delete(state.active, scannerID)
	}
	for _, worker := range state.workers {
		if worker.state != WorkerFailed && worker.state != WorkerRetired {
			worker.state = WorkerDraining
			runtime.requestWorkerShutdown(worker)
		}
	}
}

func (runtime *Runtime) allWorkersStopped(state runtimeState) bool {
	for _, worker := range state.workers {
		if !worker.exited {
			return false
		}
	}
	return true
}

func (runtime *Runtime) runtimeSnapshot(state runtimeState) RuntimeSnapshot {
	workers := make([]WorkerSnapshot, 0, len(state.workers))
	for _, worker := range state.workers {
		workers = append(workers, WorkerSnapshot{Key: worker.key, PID: worker.child.PID(), Transport: worker.transport, State: worker.state, FailureReason: worker.failureReason, ExitCode: worker.exitCode, ExitError: worker.exitError})
	}
	sort.Slice(workers, func(i, j int) bool { return workers[i].Key.String() < workers[j].Key.String() })
	return RuntimeSnapshot{Workers: workers}
}

func (runtime *Runtime) activeWorker(state runtimeState, scannerID string) activeWorkerResult {
	key, exists := state.active[scannerID]
	if !exists {
		return activeWorkerResult{err: fmt.Errorf("scanner %q has no active worker", scannerID)}
	}
	worker := state.workers[key]
	return activeWorkerResult{worker: config.Worker{ScannerID: key.ScannerID, WorkerVersion: key.Version, Transport: worker.transport, SignaturePath: worker.signaturePath}}
}

func waitRuntimeReply(ctx context.Context, done <-chan struct{}, reply <-chan error) error {
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ErrStopped
	}
}

func filepathRegular(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path must not be empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path %q: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat path %q: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("path %q is not a regular file", absolute)
	}
	return absolute, nil
}
