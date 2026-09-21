# Design Note

Short version of the reasoning behind the decisions that aren't obvious
from the code alone. See [README.md](README.md) for setup, API reference,
and the full scope/trade-off tables.

## The core problem

Two vendors describe the same physical events in incompatible shapes:
different field names, different timestamp encodings (ISO string vs. epoch
milliseconds), different units (mm/s vs. g for vibration, °C vs. °F for
temperature), different severity scales (a 4-value enum vs. a 1-5 integer),
and both send data that's sometimes missing, malformed, duplicated, or
out of order. The interesting engineering problem isn't the HTTP plumbing;
it's deciding, precisely and consistently, what to do with each kind of bad
input -- and then aggregating two vendors' opinions about the same machine
into one answer a downstream team can act on.

## Validation policy: hard-reject vs. soft-normalize

Every field a vendor sends falls into one of two buckets, applied
identically across both vendors (`internal/normalize/*.go`):

- **Hard-reject** (record dropped, never retried, reason recorded): the
  machine can't be identified (missing/unresolvable `machine_id`), the time
  can't be established (unparseable timestamp), or a core physical signal
  is outside a plausible range (e.g. `temperature_c: -999` -- a sentinel,
  not a real reading). Without these, there's nothing to normalize.
- **Soft-normalize** (record kept, issue flagged in a reason code): an
  unrecognized severity string or alert code, a level field wrong-typed
  (`"four"` instead of `4`). ThermexWatch falls back to its `alertCode`
  when `level` doesn't parse, and vice versa -- two independent severity
  signals from the same vendor, so one being garbled doesn't have to sink
  the reading.

This is a judgment call, not a spec requirement -- the brief only says
"handle" invalid values. I picked identity + time + core-signal-plausibility
as the hard line because those three are what make a record *usable at
all*; everything else degrades explainability a little without making the
record useless.

## Why vibration isn't converted across vendors

PulseForge reports vibration in mm/s (velocity); ThermexWatch reports it in
g (acceleration). These are different physical quantities -- converting
between them requires the dominant vibration frequency, which neither
vendor sends. Rather than fabricate a conversion, the canonical schema
keeps each event's vibration value in its *native* unit
(`VibrationValue`/`VibrationUnit`), and the rated-threshold check in
`rules.Derive` only compares PulseForge's mm/s against the asset
reference's `rated_max_vibration_mm_s` -- ThermexWatch's g values never
enter that comparison. Cross-vendor severity aggregation instead relies on
each vendor's own already-normalized `attention_level`, not on a unified
physical threshold. (`internal/rules/rules_test.go` has a test asserting a
numerically large `g` reading does *not* trip the mm/s threshold, precisely
to keep this from silently regressing.)

Temperature, by contrast, *is* converted (°F → °C) -- that's a valid,
lossless unit conversion, so there's no reason to keep two representations.

## machine_state vs. is_active: not the same axis

ThermexWatch's `is_active` suppresses attention (`internal/normalize/thermexwatch.go`):
`is_active: false` means the vendor is telling you the alert condition
itself has since cleared, so it shouldn't count toward current attention,
only history. It's tempting to look for the same thing in PulseForge --
its `machine_state` field (`running`/`idle`) looks like a similar kind of
status flag -- and suppress attention whenever `machine_state == "idle"`.

That would be wrong, and the seed data shows why: `PF-1004` in
`pulseforge_events.csv` is `EQ-004`, `POWER_FLUCTUATION`, `severity: medium`,
`machine_state: idle`. `is_active` and `machine_state` answer different
questions. `is_active` is a statement about *the alert* -- is the specific
condition being reported still true right now. `machine_state` is a
statement about *the machine's operating mode* -- is it currently running
or idle -- and says nothing about whether an anomaly PulseForge is
reporting alongside it is still valid. PulseForge already encodes how
concerning an event is via its own `severity` field; `machine_state`
doesn't modify that. If anything, an idle machine throwing
`HIGH_VIBRATION` or `POWER_FLUCTUATION` is a *stronger* signal, not a
weaker one -- there's no load, so readings should be quiet, and they
aren't. So PulseForge events are normalized with no equivalent
suppression, and `machine_state` is carried through as descriptive
metadata only (`event.MachineState`), the same way it arrives.

## Ordering: event_time, not arrival order

`rules.Derive` picks each vendor's "current" reading by `event_time`, not
by the order records were processed in. `vendor_api_samples/out_of_order.json`
exists specifically to test this: a `RECOVERY_SIGNAL` with a *later*
event_time arrives before a `HIGH_VIBRATION` reading with an *earlier*
event_time. If the derived view used arrival/processing order, the last
thing processed (the earlier, worse reading) would incorrectly look like
the current state. Keying on `event_time` instead means delivery order --
which vendors do not guarantee -- can't distort the answer.

## Aggregating two vendors' opinions: worst-wins

When PulseForge and ThermexWatch disagree about the same machine at
roughly the same time (`vendor_api_samples/conflicting_updates.json`), the
derived `attention_level` is the *maximum* across both vendors' latest
readings, and both contribute to `reason_codes`/`source_event_refs`. This
is the conservative, explainable choice for an "attention" signal: for a
system whose job is to flag machines that need a human to look at them,
silently averaging away a critical reading because another vendor reported
normal would be the wrong failure mode. The trade-off is a system that
runs slightly hot (more machines flagged than might strictly need it) --
acceptable for this use case, and the alternative (a weighted/ML-based
fusion) is explicitly out of scope per the brief.

## Duplicates: two dedupe keys, not one

`events` has two `UNIQUE` constraints: an exact key (`vendor,
source_event_id`) and a content key (`vendor, machine_id, event_type,
event_time`) -- see `db/schema.sql`. `vendor_api_samples/duplicates.json`
includes a case the exact key alone would miss: the same ThermexWatch
reading resent under a *new* reading id (`TW-8801-R`) -- a
retry-with-a-new-id pattern that's realistic for at-least-once vendor
delivery. `store.SaveEvent` does a plain `INSERT` and treats a Postgres
`23505` (unique_violation) on *either* constraint as "duplicate, not an
error" -- the database is the single source of truth for uniqueness, so
there's no check-then-insert race window the way there would be with an
application-level pre-check.

## Retry vs. reject: two different failure classes

The worker pool (`internal/worker/worker.go`) treats normalization
failures and storage failures differently on purpose. A normalization
reject is a property of the *data* -- retrying the exact same bad record
produces the exact same rejection, so retrying is pure waste; it's recorded
once and left alone. A sink error is a property of the *infrastructure* --
a connection blip or pool exhaustion might clear on the next attempt -- so
it's retried with backoff up to a fixed attempt count, then recorded as
`failed` (this project's dead-letter representation, queryable via the
batch status endpoint). Now that the sink is Postgres-backed, this is a
real path (a dropped connection genuinely returns an error), but it's still
awkward to trigger on demand inside a unit test without deliberately
breaking a live database mid-test -- so `worker_test.go` continues to use a
fault-injecting fake `EventSink` to exercise it deterministically, rather
than trying to simulate infrastructure failure against the real one.

## Moving from in-memory to PostgreSQL

The project started with an in-memory `store.Store` (maps behind a mutex),
documented at the time as the single biggest simplification, justified by
the brief's preference for "a simple, well-reasoned approach" over
infrastructure for its own sake. That trade-off was revisited deliberately,
not because anything about it was wrong for a first pass, but because a
real persistence layer is worth demonstrating directly rather than only
describing as a "what I'd do next."

The migration was designed to be a contained change, and it was: `store`'s
public method set (`CreateBatch`, `SetOutcome`, `GetBatch`, `SaveEvent`,
`RecomputeMachine`, `GetMachineView`, `ListMachineViews`, `PlantSummary`)
is unchanged in shape (aside from every method now taking `context.Context`
and returning an `error`, which real I/O requires honestly). `httpapi` and
`worker` already depended on `store.Store` through their own small
interfaces (`httpapi.Store`, `worker.EventSink`/`BatchTracker`), not the
concrete type, so neither package needed to change beyond threading
`context.Context` through -- the interfaces just gained a `ctx` parameter
and an `error` return where storage calls could now genuinely fail.

A few decisions worth calling out:

- **Reference data (`asset_reference.csv`/`line_reference.csv`) stayed as
  embedded CSV, not tables.** It's static, read-only, and small; there's no
  write path and no reason to pay a network round trip for a lookup that
  doesn't change at runtime. Only the *dynamic* state (batches, events,
  derived views) moved to Postgres.
- **`batch_outcomes` rows are pre-created at `CreateBatch` time** (one per
  record slot, via `generate_series`, defaulting to `status = 'queued'`),
  instead of only appearing once a record is processed. This closes a real
  gap the in-memory version had: previously, an outcome slot polled before
  the worker reached it showed an empty `status` string rather than
  `"queued"`, since the in-memory struct's zero value was never explicitly
  set. The database default fixes this for free.
- **`ingested_at` is now populated** via `DEFAULT now()` on the `events`
  table. In the in-memory version this field existed on
  `domain.NormalizedEvent` but nothing ever set it -- another small,
  pre-existing gap this migration closed incidentally rather than by
  design.
- **`machine_views` is a denormalized cache, not normalized tables.**
  `reason_codes` is a Postgres `TEXT[]`; `source_event_refs` and
  `latest_signals` are `JSONB`. This row is a point-in-time snapshot
  rebuilt wholesale by `rules.Derive` on every recompute (`INSERT ... ON
  CONFLICT (machine_id) DO UPDATE`), not something queried piecemeal or
  updated incrementally -- normalizing its sub-structures into join tables
  would add write complexity for a read pattern that never needs it. The
  history those snapshots are built from already lives, fully normalized,
  in `events`.
- **`ListMachineViews`/`PlantSummary` batch-fetch views in one query**
  (`WHERE machine_id = ANY($1)`) rather than querying per machine in a
  loop. At 8 machines the N+1 version would have been invisible in
  testing, which is exactly why it's worth avoiding as a habit rather than
  a scale-triggered fix.
- **Tests now need Docker, not nothing.** The `store` and `httpapi`
  packages start a real, ephemeral Postgres container per package via
  `testcontainers-go` (`internal/dbtest`), apply `db/schema.sql`, and
  truncate between tests -- so `go test ./...` still needs zero manual
  setup, but it does now depend on a working Docker daemon where it
  previously depended on nothing. That's a real cost of the migration, not
  hidden in the README's [Quick start](README.md#quick-start) section.
