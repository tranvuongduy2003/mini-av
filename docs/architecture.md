# MiniAV architecture

## Purpose

MiniAV is a local, educational simulation of a multi-engine antivirus system.
It demonstrates child-process isolation, pipe-based IPC, actor-style state
ownership, in-memory signature reloads, and blue/green worker replacement.

The system is not a production antivirus. It scans only harmless developer
samples or the standard EICAR test string and never downloads or executes
scanned content.

## System context

```text
User / CLI
    |
    v
+------------------------ MiniAV Core -------------------------+
|                                                               |
|  CLI transport -> Coordinator event loop -> Result aggregator |
|                         |                                     |
|                 process and IPC events                        |
|                         |                                     |
+-------------------------|-------------------------------------+
                          | NDJSON over stdin/stdout
                 +--------+--------+
                 |                 |
                 v                 v
          Scanner A process  Scanner B process
          signature store    signature store
```

Core is vendor-agnostic. Scanner-specific matching logic and signature state
live in independent worker processes.

## Components

### CLI and Core daemon

`cmd/miniav` will expose the commands in FR-01:

- `serve` starts Core and its worker set.
- `scan`, `status`, `reload`, and `update` send requests to a running Core.
- `shutdown` requests a graceful system stop.

The CLI-to-Core transport is intentionally not selected by the PRD. It must be
defined while implementing FR-01. A loopback-only TCP protocol is the portable
standard-library option; platform-native named pipes require separate Windows
and Unix implementations.

### Coordinator

`pkg/coordinator` is an actor-style control plane. Exactly one goroutine owns
and mutates:

- the scanner registry and worker lifecycle states;
- active and draining engine generations;
- routing decisions;
- in-flight requests and their expected results;
- timeout, crash, reload, and update transitions.

CLI handlers, timers, pipe readers, pipe writers, and process waiters communicate
with the coordinator through typed channels. They do not mutate coordinator
maps or worker state directly.

### Process manager

`pkg/process` starts each scanner with `os/exec`, connects its standard streams,
and observes `cmd.Wait` in a dedicated goroutine. A worker exit becomes a
coordinator event; it does not terminate Core or another worker.

### IPC and protocol

`pkg/protocol` owns Scanner Protocol v1 message definitions and validation.
`pkg/ipc` owns bounded newline-delimited JSON framing.

Each Core-to-Worker or Worker-to-Core message is one JSON object terminated by
`\n`. Worker stdout is reserved for protocol frames. Human-readable and
structured diagnostics go to stderr through `log/slog` so they cannot corrupt
the protocol stream.

### Signature engine

`pkg/signatures` loads UTF-8 text databases, ignores empty and comment lines,
and removes duplicates. Workers scan files as read-only streams for literal
matches.

A reload is prepared and validated as a new immutable database before it is
published. Failed preparation leaves the current database untouched. Scans
already holding the old database may finish against that immutable snapshot.

### Result aggregation

`pkg/aggregator` applies the `ANY_MALICIOUS` policy:

1. Any `MALWARE` result produces `MALWARE`.
2. Otherwise, any `ERROR`, `TIMEOUT`, or `UNAVAILABLE` produces
   `INCONCLUSIVE`.
3. All `CLEAN` results produce `CLEAN`.

Aggregation is a pure policy and does not own process or coordinator state.

### Update manager

`pkg/update` reads local manifests, validates required metadata and SHA-256,
and stages candidate artifacts. It never changes active routing by itself;
activation is a coordinator state transition after the candidate has passed
startup, protocol handshake, and health checks.

Relative-path resolution is not defined by the PRD. FR-07 must choose one
deterministic base—preferably the configuration file directory or an explicit
data directory—and apply it consistently to manifests, artifacts, signatures,
and runtime data.

## Worker lifecycle

```text
STARTING -> HEALTHY -> ACTIVE -> DRAINING -> RETIRED
    |          |          |          |
    +----------+----------+----------+-> FAILED
```

- `STARTING`: process exists but is not eligible for scans.
- `HEALTHY`: handshake and health checks succeeded.
- `ACTIVE`: receives new scan requests for its scanner slot.
- `DRAINING`: receives no new work but finishes assigned requests.
- `RETIRED`: drained and shut down successfully.
- `FAILED`: crashed, failed health, timed out during activation, or exited
  unexpectedly.

## Main flows

### Scan

1. Core validates that the target is a regular file.
2. The coordinator allocates a request ID and records the active worker set.
3. IPC writers send the scan request without blocking the event loop.
4. Reader goroutines turn worker responses into coordinator events.
5. The coordinator collects all expected results or substitutes terminal
   timeout/unavailable results.
6. The aggregator computes the user-visible verdict.

### Signature reload

1. Core sends a reload request to the selected active worker.
2. The worker prepares and validates a new immutable database.
3. On success it atomically publishes the database and acknowledges the new
   count; on failure it retains the old database and returns an error.

### Blue/green update

1. Validate the manifest and candidate artifact hash.
2. Start the candidate without changing active routing.
3. Complete protocol handshake and health checks.
4. Make the candidate active and mark the previous worker draining as one
   coordinator transition.
5. Route new scans to the candidate while old requests finish on the previous
   worker.
6. Shut down and retire the old worker after its in-flight count reaches zero.

Any failure before activation leaves the previous worker active. A candidate
failure after activation is reported to the coordinator so it can apply the
rollback policy defined by FR-07.

## Concurrency and shutdown rules

- The coordinator must never block on pipe I/O, process waits, filesystem
  scans, or timer sleeps.
- Channel producers own sending; the component that creates a channel owns
  closing it. Receivers do not close shared event channels.
- Every operation that can wait accepts a `context.Context` or has a bounded
  timeout.
- Graceful shutdown stops new requests, resolves or cancels in-flight work,
  asks workers to stop, and reaps every child process.
- Tests must exercise cancellation and unexpected exits under `go test -race`.

## Security and portability boundaries

- Use Go 1.22 or newer and the standard library only.
- Bind any local network control endpoint to loopback, never all interfaces.
- Treat file paths and protocol data as untrusted input.
- Never execute scanned files or obtain real malware for tests.
- Prefer portable APIs; isolate unavoidable operating-system behavior in small
  build-tagged files.
- Keep release and staging data local and validate before activation.
