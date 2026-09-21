# MiniAV architecture

## Purpose

MiniAV is a local, educational simulation of a multi-engine antivirus system.
It demonstrates child-process isolation, three-transport IPC, actor-style state
ownership, in-memory signature reloads, and blue/green worker replacement.

The system is not a production antivirus. It scans only harmless developer
samples or the standard EICAR test string and never downloads or executes
scanned content.

## System context

```text
Docker host (loopback only)
  publish script -> release files -> nginx :8080
                                      |
                                      | HTTP + SHA-256 manifest
                                      v
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
                          | stdio / TCP socket / gRPC
                 +--------+--------+--------+
                 |                 |        |
                 v                 v        v
          Scanner A process  Scanner B process  Scanner C process
          stdio + store      socket + store     gRPC + store
```

Core is vendor-agnostic. Scanner-specific matching logic and signature state
live in independent worker processes.

## Components

### CLI and Core daemon

`cmd/miniav` will expose the commands in FR-01:

- `serve` starts Core and its worker set.
- `scan`, `status`, `reload`, and `update` send requests to a running Core.
- `shutdown` requests a graceful system stop.

The CLI-to-Core transport is newline-delimited JSON over TCP at
`127.0.0.1:7331`. The listener accepts only a literal loopback IP, each
connection carries one command and one response, frames are limited to 64 KiB,
and requests have a bounded deadline. This keeps the control path portable and
within the standard library without conflating it with Scanner Protocol v1.

FR-01 implements Core liveness/status and graceful shutdown on this transport.
The `serve` configuration is strict JSON with positive `startupTimeoutMs` and
`scanTimeoutMs` values and exactly three required worker slots: `stdioWorker`,
`socketWorker`, and `grpcWorker`. Each slot defines a unique `scannerId`,
`workerVersion`, executable path, signature path, and optional fault-injection
settings. Relative paths resolve from the directory containing the
configuration file. Unknown fields, duplicate worker IDs, missing slots, and
non-regular executable or signature paths are rejected before Core listens.

### Coordinator

`pkg/coordinator` is an actor-style control plane. Exactly one goroutine owns
and mutates:

- the scanner registry and worker lifecycle states;
- active and draining engine generations;
- routing decisions;
- in-flight requests and their expected results;
- timeout, crash, reload, and update transitions.

CLI handlers, timers, transport readers, transport writers, and process waiters communicate
with the coordinator through typed channels. They do not mutate coordinator
maps or worker state directly.

### Process manager

`pkg/process` starts each scanner with `os/exec`, exposes its standard streams,
and observes `cmd.Wait` in a dedicated goroutine. A worker exit becomes a
coordinator event; it does not terminate Core or another worker.

### IPC and protocol

`pkg/protocol` owns Scanner Protocol v1 message definitions and validation.
`pkg/ipc` owns bounded newline-delimited JSON framing. `pkg/workertransport`
owns the Core-side transport connection.

The topology fixes one transport per worker slot. `stdioWorker` uses NDJSON
over child standard streams. `socketWorker` uses the same bounded NDJSON frames
over a loopback TCP connection. `grpcWorker` uses the generated
`miniav.scanner.v1.Scanner/Exchange` bidirectional service with Protocol
Buffers messages defined in `api/scanner/v1/scanner.proto`. Socket and gRPC
workers bind an ephemeral loopback port and announce it to Core with one
bootstrap frame on stdout. Human-readable and structured diagnostics always go
to stderr through `log/slog`.

`pkg/worker` implements the shared Scanner Protocol v1 runtime used by
`cmd/scanner-a`, `cmd/scanner-b`, and `cmd/scanner-c`. Each command fixes its
scanner identity and transport, while flags provide the worker version, initial
signature database, and documented fault-injection behavior. The worker accepts
only Core-to-Worker message types, handles scans concurrently, serializes
protocol responses, and waits for in-flight scans before acknowledging shutdown.

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

`pkg/update` reads manifests from either the local filesystem or a loopback
HTTP(S) URL, validates required metadata and SHA-256, and stages candidate
artifacts. Remote manifest artifacts are resolved relative to the manifest URL
and must remain on the same origin. Remote requests do not follow redirects;
manifest and artifact bodies are bounded to 64 KiB and 128 MiB respectively.
Downloads stream through SHA-256 verification into a temporary staging file
and are atomically published only after validation. The package never changes
active routing by itself; activation is a coordinator state transition after
the candidate has passed startup, protocol handshake, and health checks.

Relative manifest and artifact paths resolve from the directory containing the
Core configuration file; absolute paths remain absolute. Validated artifacts
are published immutably beneath
`staging/<scanner-id>/<worker-version>/<artifact-name>` in that directory.
Other runtime path-bearing features must use the same base when they are wired
into Core.

### Local update server

`update-server/compose.yaml` runs a single nginx container. Its port is
published only as `127.0.0.1:8080`, and a read-only bind mount exposes generated
release files beneath `/releases/`. nginx permits only `GET` and `HEAD`, disables
directory listing, and provides no upload or mutation API.

`update-server/publish.ps1` is the deliberately small vendor integration
pipeline for the demo: it builds a selected worker, places the artifact in a
versioned release directory, calculates SHA-256, and writes the release
manifest. There is no adapter service, signing service, CDN, TLS termination,
or metadata signature. This is a local distribution fixture, not a production
update service.

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
2. The coordinator allocates a request ID and records the three active workers.
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

1. Read a local manifest or download a loopback manifest and its artifact.
2. Validate the manifest and candidate artifact hash, then publish staging.
3. Start the candidate without changing active routing.
4. Complete protocol handshake and health checks.
5. Make the candidate active and mark the previous worker draining as one
   coordinator transition.
6. Route new scans to the candidate while old requests finish on the previous
   worker.
7. Shut down and retire the old worker after its in-flight count reaches zero.

Any failure before activation marks only the candidate failed and leaves the
previous worker active. After activation, an unexpected candidate exit follows
the normal failure-isolation path: the active route is removed and pending
results become unavailable. FR-07 does not reactivate the draining worker after
cutover.

## Concurrency and shutdown rules

- The coordinator must never block on transport I/O, process waits, filesystem
  scans, or timer sleeps.
- Transport reader and writer goroutines never mutate coordinator state.
- Channel producers own sending; the component that creates a channel owns
  closing it. Receivers do not close shared event channels.
- Every operation that can wait accepts a `context.Context` or has a bounded
  timeout.
- Graceful shutdown stops new requests, resolves or cancels in-flight work,
  asks workers to stop, and reaps every child process.
- Tests must exercise cancellation and unexpected exits under `go test -race`.

## Security and portability boundaries

- Use Go 1.22 or newer. The only direct third-party dependencies are
  `google.golang.org/grpc` and `google.golang.org/protobuf`.
- Bind any local network control endpoint to loopback, never all interfaces.
- Bind worker Socket and gRPC endpoints to loopback with ephemeral ports.
- Accept remote update sources only when the URL host is a literal loopback IP;
  reject redirects and cross-origin artifact references.
- Treat file paths and protocol data as untrusted input.
- Never execute scanned files or obtain real malware for tests.
- Prefer portable APIs; isolate unavoidable operating-system behavior in small
  build-tagged files.
- Keep release and staging data local and validate before activation.
