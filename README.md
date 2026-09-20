# MiniAV

**A local, hot-swappable malware scanning platform for learning systems
engineering in Go.**

MiniAV coordinates independent scanner processes behind a stable Core. Scanner
engines and signature databases can be replaced while Core stays online,
in-flight scans finish safely, and failed candidates leave the working engine
in place. The project explores process isolation, NDJSON IPC, actor-style
concurrency, health-gated activation, graceful draining, and rollback.

> [!WARNING]
> MiniAV is a learning project, not production antivirus software. It does not
> provide real-time protection and must never be used with real malware. Tests
> should use harmless developer-defined strings or the standard EICAR test
> string only.

## Project status

FR-01 through FR-05 are implemented. The `miniav` binary provides the complete
command surface, a loopback-only Core control endpoint, status reporting, and
graceful shutdown. The protocol and IPC packages provide validated Scanner
Protocol v1 messages, bounded NDJSON framing, and the five defined per-worker
verdicts. The process and coordinator packages provide child-process spawning,
exit observation, isolated worker failure state, and `UNAVAILABLE` results for
pending worker requests. The signatures package provides UTF-8 database
loading, literal streaming matches, immutable snapshots, and atomic reloads.

Worker executables, worker configuration, scan routing and aggregation,
timeouts, Core-to-worker reload routing, and worker updates remain pending in
later functional requirements. The `scan`, `reload`, and `update` commands
therefore continue to return a clear unavailable error from Core.

## Hot-swap model

Each scanner slot has one active worker. An update follows a blue/green
lifecycle:

1. Verify the candidate artifact's SHA-256 digest.
2. Start the candidate beside the active worker.
3. Require a successful protocol handshake and health check.
4. Route new scans to the candidate in one coordinator transition.
5. Let the previous worker finish its assigned scans before shutting it down.
6. Keep or restore the last healthy worker if activation fails.

Core keeps the same process ID throughout this lifecycle. Scanner crashes are
contained to their child process and converted into explicit scan or update
results.

## Platform goals

- Upgrade scanner workers without restarting Core or losing in-flight work.
- Reject unhealthy releases before they receive scan traffic.
- Isolate every scanner in an independently managed child process.
- Scan with multiple engines and combine their results deterministically.
- Reload signatures without restarting Core or a worker.
- Exchange newline-delimited JSON messages over standard input and output.
- Keep routing and lifecycle state inside a single coordinator goroutine.
- Use only the Go standard library.

## Architecture

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

Core is scanner-agnostic. Each worker owns its matching logic and in-memory
signature database, while the coordinator owns worker state, routing,
in-flight requests, timeouts, and update transitions. A crashed worker cannot
directly terminate Core or another worker.

See [the architecture guide](docs/architecture.md) for component boundaries,
worker lifecycle states, and concurrency rules.

## CLI

```text
miniav serve --config <path>
miniav scan <file>
miniav status
miniav reload <scanner-id> <signature-path>
miniav update --manifest <path>
miniav shutdown
```

| Command | Purpose |
| --- | --- |
| `serve` | Start Core and its configured worker processes. |
| `scan` | Scan a regular file with all active workers. |
| `status` | Show Core and worker health and lifecycle state. |
| `reload` | Replace one worker's in-memory signature database. |
| `update` | Validate and activate a new worker release. |
| `shutdown` | Gracefully stop Core and reap all child processes. |

Start Core with an existing regular file as the configuration path:

```sh
go run ./cmd/miniav serve --config <path>
```

Alternatively, build the platform-specific `miniav` executable in the
repository root:

```sh
make build
```

In another terminal, query or stop the running Core:

```sh
go run ./cmd/miniav status
go run ./cmd/miniav shutdown
```

Core listens on the loopback-only control address `127.0.0.1:7331`. FR-01
reserves configuration parsing for the requirement that defines the schema, so
the current `serve` command validates that the supplied path is a regular file
without interpreting its contents.

## Development commands

```sh
make fmt
make vet
make test
make test-race
make check
```

`make check` formats the Go source and runs every required verification gate.
Run any CLI command by passing its arguments through `ARGS`:

```sh
make run ARGS="status"
make run ARGS="serve --config <path>"
```

## Scanner protocol

Core and workers use Scanner Protocol v1: one JSON object per line over
`stdin` and `stdout`. Worker diagnostics go to `stderr` so logs cannot corrupt
the protocol stream. Frames are limited to 64 KiB and messages with an unknown
field, unsupported protocol version, unknown type, missing required field, or
missing terminating newline are rejected.

Example request:

```json
{"version":1,"type":"SCAN","request_id":101,"file_path":"C:\\samples\\clean.txt"}
```

Example response:

```json
{"version":1,"type":"SCAN_RESULT","request_id":101,"verdict":"CLEAN","duration_ms":3}
```

The protocol also defines messages for handshake, health checks, signature
reloads, and graceful shutdown. `HELLO_ACK` uses `version` for the numeric
protocol version and `worker_version` for the worker release version.

Complete example streams are available in
[`samples/protocol-v1/core-to-worker.ndjson`](samples/protocol-v1/core-to-worker.ndjson)
and
[`samples/protocol-v1/worker-to-core.ndjson`](samples/protocol-v1/worker-to-core.ndjson).

## Verdicts and aggregation

Each `SCAN_RESULT` carries exactly one of the per-worker verdicts `CLEAN`,
`MALWARE`, `ERROR`, `TIMEOUT`, or `UNAVAILABLE`. Scanner Protocol v1 rejects
other verdict values.

The planned FR-06 runtime aggregation uses an `ANY_MALICIOUS` policy:

1. If any worker returns `MALWARE`, the combined verdict is `MALWARE`.
2. Otherwise, if any worker returns `ERROR`, `TIMEOUT`, or `UNAVAILABLE`, the
   combined verdict is `INCONCLUSIVE`.
3. If every worker returns `CLEAN`, the combined verdict is `CLEAN`.

`INCONCLUSIVE` is a combined result rather than a per-worker protocol verdict.
The aggregation runtime is not implemented yet.

Scanned files are opened read-only and are never executed.

## Signature reloads

A signature database is a UTF-8 text file containing one literal pattern per
line. Blank lines and lines beginning with `#` are ignored, and duplicate
patterns are removed. Pattern whitespace and case are significant. Reloading
prepares and validates a new immutable database before atomically publishing
it; a failed reload leaves the current database unchanged. A scan already in
progress finishes against the immutable snapshot it acquired before a reload.

The signature store is implemented as a reusable worker component. The Core
`reload` command remains unavailable until worker startup and routing are
implemented.

## Worker releases

Worker releases are described by local JSON manifests containing the scanner
ID, worker version, signature version, artifact path, and SHA-256 digest. Core
validates the artifact before starting it. A candidate must pass startup,
protocol handshake, and health checks before it receives new scans. The old
worker drains its in-flight requests before shutdown, and remains active if
the candidate fails before activation.

## Requirements

- Go 1.22 or newer
- Windows x64, Linux, or macOS
- No third-party Go modules or native toolchain dependencies

Once implementation packages are present, the standard verification commands
will be:

```sh
gofmt -w <changed-go-files>
go vet ./...
go test ./...
go test -race ./...
```

## Roadmap

- [ ] Standalone scanner
- [x] Signature database, streaming matching, and atomic reload
- [x] Scanner Protocol v1 over standard streams
- [x] Child-process supervision and crash isolation
- [ ] Coordinator and multi-worker aggregation
- [ ] Blue/green worker updates and rollback
- [ ] End-to-end local demonstration

## Non-goals

MiniAV intentionally excludes real-time protection, kernel or driver work,
memory and network scanning, archive extraction, a graphical interface,
Internet-hosted updates, and production security guarantees.
