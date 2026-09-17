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

`store.SaveEvent` checks both an exact key (`vendor + source_event_id`) and
a content key (`vendor + machine_id + event_type + event_time`).
`vendor_api_samples/duplicates.json` includes a case the exact key alone
would miss: the same ThermexWatch reading resent under a *new* reading id
(`TW-8801-R`) -- a retry-with-a-new-id pattern that's realistic for
at-least-once vendor delivery. Content-key matching catches it without
needing a real hash; the composite string is deliberately human-readable
for debugging.

## Retry vs. reject: two different failure classes

The worker pool (`internal/worker/worker.go`) treats normalization
failures and storage failures differently on purpose. A normalization
reject is a property of the *data* -- retrying the exact same bad record
produces the exact same rejection, so retrying is pure waste; it's recorded
once and left alone. A sink error is (in a real system) a property of the
*infrastructure* -- a DB write timeout might succeed on the next attempt --
so it's retried with backoff up to a fixed attempt count, then recorded as
`failed` (this project's dead-letter representation, queryable via the
batch status endpoint). The in-memory store used here never actually fails
a write, so that path is only exercised by a fault-injecting fake in
`worker_test.go` -- an honest gap, called out rather than hidden, since a
real deployment's store could fail and this is the mechanism that would
catch it.

## In-memory storage

The single biggest simplification in this project. Chosen because the
brief explicitly prefers "a simple, well-reasoned approach" over
infrastructure for its own sake, and because none of the interesting
engineering here (normalization policy, aggregation logic, async
processing, dedupe) needs a real database to demonstrate. The cost is
explicit and listed in the README: data doesn't survive a restart, and
there's no horizontal scaling story. `store.Store`'s method set is the only
thing the HTTP and worker layers depend on (both take it as a small
interface, not the concrete type), so replacing it with a Postgres-backed
implementation later is a contained change, not a rewrite.
