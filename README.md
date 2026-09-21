# Aurik Equipment Monitor

A small backend service for an industrial equipment monitoring platform. It
ingests machine sensor/alert data from two vendors with different schemas,
normalizes it into one canonical internal representation, processes it
asynchronously, and serves a derived **machine operational attention view**
plus plant-level summaries.

Built for the Aurik Technologies Tech Lead take-home assessment. Deliberately
kept small: two vendors (not three), no auth. Storage is PostgreSQL --
see [db/schema.sql](db/schema.sql) for the schema. See
[Scope & trade-offs](#scope--trade-offs) for why, and
[DESIGN_NOTE.md](DESIGN_NOTE.md) for the reasoning behind the less obvious
decisions.

## Architecture

```mermaid
flowchart LR
    PF[PulseForge vendor] -->|POST /v1/ingest/pulseforge| API
    TW[ThermexWatch vendor] -->|POST /v1/ingest/thermexwatch| API

    subgraph Service
        API[HTTP API] -->|per-record job| Queue[(buffered channel)]
        Queue --> Worker[worker pool]
        Worker -->|normalize| Normalize[normalize package]
        Normalize -->|dedupe + save| Store[(PostgreSQL)]
        Store -->|recompute| Rules[rules.Derive]
        Rules --> Store
        RefData[[asset/line reference CSVs]] --> Normalize
        RefData --> Rules
    end

    API -->|GET batch status| Store
    API -->|GET machine view| Store
    API -->|GET plant summary| Store
    Store --> API
```

**Request path (synchronous):** the HTTP handler reads a vendor batch,
splits it into individual raw records, creates a `Batch` with one pending
outcome slot per record, and enqueues a job per record. It responds `202`
with a `batch_id` as soon as the envelope itself parses -- it does not wait
for normalization.

**Processing path (asynchronous):** a fixed pool of worker goroutines drains
the job queue. For each job: normalize the raw record into the canonical
schema (or reject it, if it's unusable) → check it isn't a duplicate → save
it → recompute that machine's derived view. Every step updates the record's
outcome, visible via the batch status endpoint.

## Quick start

### Docker Compose (preferred)

```bash
docker compose up --build
```

Starts Postgres (schema applied automatically from `db/schema.sql` on first
boot, via `/docker-entrypoint-initdb.d`) and the API, in that order --
`equipment-monitor` waits on Postgres's healthcheck before starting. The API
is at `http://localhost:8080`.

### Locally, without Docker

Requires Go 1.25+ and a running Postgres.

```bash
createdb aurik_equipment_monitor
psql aurik_equipment_monitor -f db/schema.sql

export DATABASE_URL='postgres://aurik:aurik@localhost:5432/aurik_equipment_monitor?sslmode=disable'
go run ./cmd/server
# listens on :8080; set PORT to change it
```

(Or point `DATABASE_URL` at any Postgres you already have -- the schema
doesn't assume any particular user/db name, those are just what the example
above and docker-compose.yml use.)

### Running the tests

```bash
go test ./...          # all packages
go test ./... -race     # with the race detector (worker pool is concurrent)
```

36 tests across normalization, the rules engine, the store, the worker pool,
and full HTTP integration (real store + real worker pool, no fakes). The
store and httpapi packages need a working **Docker daemon** to run --
they start an ephemeral, schema-loaded Postgres container per package via
[testcontainers-go](internal/dbtest/dbtest.go) (one container per package,
reused and truncated between tests in it, not one per test). No manual
database setup needed for `go test` itself; normalize/rules/worker have no
such dependency and run instantly.

## API

All endpoints are prefixed `/v1` except health. Every response is JSON.

A ready-to-import Postman collection is at
[Aurik-Equipment-Monitor.postman_collection.json](Aurik-Equipment-Monitor.postman_collection.json)
-- 15 requests covering every endpoint below, including the malformed/
duplicate/rejected edge cases, each with an inline test assertion. Import
it, set the `base_url` collection variable if you're not on `:8080`, and
run top to bottom.

| Method & path | Purpose |
|---|---|
| `GET /healthz` | Liveness check. |
| `POST /v1/ingest/pulseforge` | Accepts one PulseForge batch envelope. |
| `POST /v1/ingest/thermexwatch` | Accepts one ThermexWatch batch envelope. |
| `GET /v1/ingest/batches/{batchID}` | Per-record processing status for a batch. |
| `GET /v1/machines/{machineID}` | Derived operational attention view for one machine. |
| `GET /v1/machines?plant_id=&status=` | All machines' views, optionally filtered. |
| `GET /v1/plants/{plantID}/summary` | Plant/line rollup: counts by status, critical machines, stale machines. |

### Ingest a PulseForge batch

```bash
curl -s -X POST localhost:8080/v1/ingest/pulseforge \
  -H 'Content-Type: application/json' \
  -d '{
    "vendor": "PulseForge", "plant_id": "PLANT_01", "batch_generated_at": "2026-04-18T08:05:00Z",
    "events": [{
      "event_id": "PF-1001", "machine_id": "EQ-001", "line_id": "LINE-A",
      "event_time": "2026-04-18T07:59:12Z", "event_type": "HIGH_VIBRATION", "severity": "high",
      "vibration_mm_s": 11.8, "temperature_c": 83.2, "machine_state": "running",
      "sensor_health": 0.91, "vendor_confidence": 0.87
    }]
  }'
# -> 202 {"batch_id":"batch_...","status":"queued","status_url":"/v1/ingest/batches/batch_..."}
```

### Check what happened to it

```bash
curl -s localhost:8080/v1/ingest/batches/batch_XXXXXXXXXXXX
```

### Read the derived view

```bash
curl -s localhost:8080/v1/machines/EQ-001
curl -s localhost:8080/v1/plants/PLANT_01/summary
```

The request/response envelope shapes match `vendor_api_samples/pulseforge_sample.json`
and `thermexwatch_sample.json` from the assessment assets exactly (post the
inner object directly as the body -- the assets' `happy_path.json` and
edge-case files nest that same shape under a `"pulseforge"`/`"thermexwatch"`
key purely for bundling multiple vendors in one local test file; pull the
relevant key out with `jq` before posting it, e.g.
`jq .pulseforge happy_path.json | curl -d @- ...`).

## Canonical schema & the machine view

Every vendor record normalizes to `domain.NormalizedEvent`
([internal/domain/types.go](internal/domain/types.go)): machine/plant/line
id (resolved against reference data, not trusted from the vendor), a UTC
`event_time`, the vendor's own event/alert code (kept, for traceability), a
normalized `attention_level` (none/low/medium/high/critical -- the one
severity scale every vendor's own severity/level field is mapped onto), and
whichever physical signals the vendor reported.

`GET /v1/machines/{id}` returns `domain.MachineView`, matching the fields
requested in `expected_output_context.md`:

```json
{
  "machine_id": "EQ-001", "plant_id": "PLANT_01", "line_id": "LINE-A",
  "derived_status": "needs_attention",
  "needs_attention": true,
  "attention_level": "high",
  "reason_codes": ["PULSEFORGE_HIGH_VIBRATION_HIGH", "VIBRATION_ABOVE_RATED_MAX"],
  "latest_relevant_event_time": "2026-04-18T07:59:12Z",
  "processing_status": "stale",
  "last_processed_at": "2026-09-17T05:25:21Z",
  "source_event_refs": [{"vendor": "pulseforge", "source_event_id": "PF-1001"}],
  "latest_signals": [{"vendor": "pulseforge", "event_type": "HIGH_VIBRATION", "event_time": "...", "attention_level": "high"}]
}
```

How it's computed (deterministic, no ML -- see [DESIGN_NOTE.md](DESIGN_NOTE.md)
for the full reasoning): take each vendor's latest event *by event_time*
(not arrival order), take the worst `attention_level` across vendors,
cross-check against the machine's rated thresholds for additional reason
codes, and layer the asset's maintenance status on top as the final
`derived_status`.

## Scope & trade-offs

Kept deliberately small, per the brief's "do not over-engineer" guidance.
What's in vs. out, and why:

| Included | Why |
|---|---|
| 2 vendors (PulseForge, ThermexWatch) | Meets the brief's stated minimum; enough schema divergence (units, timestamp formats, severity scales, camelCase vs. snake_case) to demonstrate real normalization. |
| PostgreSQL storage | Batches/outcomes, events, and derived views all persist across restarts. Idempotency is enforced by real `UNIQUE` constraints (see [db/schema.sql](db/schema.sql)), not application-level maps. |
| Async worker pool (goroutines + channel) | Real concurrency, real retry/dead-letter semantics, no external broker to run. |
| Per-record accept/reject, not whole-batch | One malformed record in a batch doesn't sink the rest -- this is the actual point of the exercise. |
| No auth | Explicitly optional per the brief ("if you choose to include it"); would add API-key or mTLS auth in production. |

| Deliberately excluded | What I'd do instead in production |
|---|---|
| A third vendor (MaintaFlow) | Same normalization pattern extends directly; skipped to keep the codebase small enough to walk through in an interview. |
| API versioning beyond a `/v1` prefix | Add a version negotiation strategy once there's a second version to negotiate. |
| Durable job queue | `batch_outcomes` rows now persist and correctly show `'failed'` after retries are exhausted -- but the retry loop itself is still in-process: a crash mid-retry leaves that record's row stuck at `'queued'` forever, with nothing to redeliver it (the in-memory Go channel job is gone). A production system would need either a durable queue (e.g. a message broker) or a reconciliation sweep that finds stale `'queued'` rows on startup and re-enqueues them. |
| Migration tooling | `db/schema.sql` is a single hand-run script, not versioned migrations. Fine for one schema, would move to `golang-migrate`/`goose`/etc. the moment the schema needs to evolve without a full recreate. |
| Rate limiting / backpressure beyond a blocking channel | `Pool.Submit` blocks if the queue is full; production would return `503` instead. |
| Distributed processing | A single process with an in-memory queue is adequate at this scale; the brief explicitly asks for "a simple and well-reasoned approach," not a distributed system. |

## Limitations

- **The retry loop doesn't survive a restart** (see the "durable job queue"
  row above) -- persisted status did not automatically mean persisted
  in-flight work; that distinction is worth being explicit about.
- **Freshness window is a fixed 2 hours,** not configurable, not
  machine/criticality-aware. A `high`-criticality press probably deserves a
  tighter staleness threshold than a `low`-criticality packager.
- **No pagination** on `GET /v1/machines` -- fine at 8 machines, not at
  8,000. `ListMachineViews`/`PlantSummary` already batch-fetch views in one
  query (`= ANY($1)`), so adding `LIMIT`/`OFFSET` later doesn't require
  touching that part.
- **Single-process worker pool.** The Postgres store itself would happily
  serve multiple app replicas (it's just SQL), but the in-memory job
  channel is per-process, so today only one replica should run.
- **Vibration is not cross-vendor-comparable** by design (see DESIGN_NOTE) --
  a consumer wanting a single normalized vibration number across vendors
  would need additional vendor calibration data this assessment doesn't
  provide.

## What I'd do next for production

1. Move retry/redelivery to a durable queue (or add a startup reconciliation
   sweep for stuck `'queued'` rows) so a crash mid-processing can't silently
   strand a record -- see "Durable job queue" above.
2. Adopt migration tooling (`golang-migrate`/`goose`) once the schema needs
   to evolve incrementally instead of via a full recreate.
3. Add API-key or mTLS authentication on the ingestion endpoints, and rate
   limiting per vendor.
4. Make the freshness window and attention thresholds configurable per
   machine criticality rather than one fixed constant.
5. Add structured logging/tracing and basic metrics (queue depth, per-vendor
   reject rate, processing latency) -- the things you'd actually want paged
   on.
6. Add OpenAPI generation from the handler layer (currently hand-documented
   in this README) so the contract can't drift from the implementation.

## AI usage disclosure

Built with Claude Code (Anthropic). It wrote the initial implementation of
every layer (normalization, rules engine, store, worker pool, HTTP API) and
the tests, from a scope and set of validation-policy decisions worked out
in conversation first (which fields are hard-reject vs. soft-normalize,
the worst-wins aggregation approach, event-time- vs. arrival-order-based
recency, why vibration units aren't cross-vendor-converted, and the
two-vendor/no-auth minimal scope). Storage started in-memory for the
initial submission and was deliberately migrated to PostgreSQL afterward
(schema hand-specified first, store rewritten against it, everything
re-verified against a real database via testcontainers-go before commit)
-- both the trade-off and the migration were explicit decisions, not a
default. All of it was reviewed, built, and test-run locally against the
assessment's sample and edge-case data before being committed; nothing
here is unreviewed generated output.
