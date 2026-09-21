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

FR-01 through FR-07 are implemented end to end. Core starts configured worker
processes, gates activation on handshake and health checks, scans with every
active engine, substitutes timeout and unavailable results, reloads signatures,
and performs health-gated blue/green updates from SHA-256-verified local
manifests. Worker crashes remain isolated and pre-activation update failures
leave the active version in place.

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

Build Core and both workers, then start Core with the supplied configuration:

```sh
make build
./miniav serve --config config/miniav.json
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

Core listens on the loopback-only control address `127.0.0.1:7331`. On a
non-Windows host, remove the `.exe` suffixes from the executable paths in the
sample configuration. Relative paths in configuration, scan, reload, and
manifest commands resolve from the configuration directory.

The strict JSON configuration contains positive `startupTimeoutMs` and
`scanTimeoutMs` values plus a non-empty worker list. Workers also accept
optional `delayMs`, `crashOnScan`, and `failHealth` fields for local fault
injection.

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

The `aggregator` package implements the `ANY_MALICIOUS` policy:

1. If any worker returns `MALWARE`, the combined verdict is `MALWARE`.
2. Otherwise, if any worker returns `ERROR`, `TIMEOUT`, or `UNAVAILABLE`, the
   combined verdict is `INCONCLUSIVE`.
3. If every worker returns `CLEAN`, the combined verdict is `CLEAN`.

`INCONCLUSIVE` is a combined result rather than a per-worker protocol verdict.
Aggregation rejects an empty set, pending results, and unknown verdicts.

Scanned files are opened read-only and are never executed.

## Signature reloads

A signature database is a UTF-8 text file containing one literal pattern per
line. Blank lines and lines beginning with `#` are ignored, and duplicate
patterns are removed. Pattern whitespace and case are significant. Reloading
prepares and validates a new immutable database before atomically publishing
it; a failed reload leaves the current database unchanged. A scan already in
progress finishes against the immutable snapshot it acquired before a reload.

The `reload` command routes the new path to the selected active worker and
reports the published signature count.

## Worker releases

Worker releases are described by local JSON manifests containing the scanner
ID, worker version, signature version, artifact path, and SHA-256 digest.
Relative manifest and artifact paths resolve from the directory containing the
Core configuration file; absolute paths remain absolute. A validated artifact
is published beneath
`staging/<scanner-id>/<worker-version>/<artifact-name>` in that configuration
directory. Matching staged content is reusable, while conflicting content is
never overwritten.

Core must prepare and hash a release before starting it. A candidate becomes
eligible for activation only after its runtime reports successful handshake and
health events to the coordinator. Startup, handshake, health, or timeout
failure marks only the candidate failed and leaves the previous worker active.
After activation, the old version drains its assigned requests before
retirement. An unexpected exit after activation follows normal failure
isolation and does not reactivate an already draining generation.

## Local demonstration

Run the complete scenario from PowerShell:

```powershell
.\demo.ps1
```

The script builds all binaries, starts two workers, demonstrates clean and
malicious aggregation, reloads Scanner A, performs an in-flight v1-to-v2
cutover, rejects a broken v3 artifact while retaining v2, verifies the stable
Core PID, and shuts the system down.

## Requirements

- Go 1.22 or newer
- Windows x64, Linux, or macOS
- No third-party Go modules or native toolchain dependencies

The standard verification commands are:

```sh
gofmt -w <changed-go-files>
go vet ./...
go test ./...
go test -race ./...
```

## Roadmap

- [x] Standalone scanner
- [x] Signature database, streaming matching, and atomic reload
- [x] Scanner Protocol v1 over standard streams
- [x] Child-process supervision and crash isolation
- [x] Coordinator state tracking and result aggregation policy
- [x] Manifest validation, immutable staging, and pre-activation rollback state
- [x] Worker runtime and multi-worker scan routing
- [x] Blue/green worker updates and rollback
- [x] End-to-end local demonstration

## Non-goals

MiniAV intentionally excludes real-time protection, kernel or driver work,
memory and network scanning, archive extraction, a graphical interface,
Internet-hosted updates, and production security guarantees.
