-- Aurik Equipment Monitor -- PostgreSQL schema
--
-- Run against a fresh database:
--   createdb aurik_equipment_monitor
--   psql aurik_equipment_monitor -f db/schema.sql
--
-- (docker-compose applies this automatically on first container start via
-- /docker-entrypoint-initdb.d -- see docker-compose.yml.)

-- ============================================================
-- batches: one row per ingestion HTTP call.
-- ============================================================

CREATE TABLE batches (
    id           TEXT PRIMARY KEY,
    vendor       TEXT NOT NULL CHECK (vendor IN ('pulseforge', 'thermexwatch')),
    received_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    record_count INT NOT NULL CHECK (record_count >= 0)
);

-- ============================================================
-- events: canonical normalized events -- the source of truth
-- the rules engine derives every machine view from.
--
-- Idempotency is enforced by two independent uniqueness checks,
-- replacing the two in-memory dedupe maps 1:1:
--   - (vendor, source_event_id): the vendor's own record id.
--   - (vendor, machine_id, event_type, event_time): catches a
--     vendor retry sent under a brand new record id (see
--     DESIGN_NOTE.md's "duplicates.json" example).
-- A plain INSERT that violates either constraint is treated as
-- a duplicate by the application (Postgres error 23505), not a
-- failure.
-- ============================================================
CREATE TABLE events (
    id               TEXT PRIMARY KEY,
    vendor           TEXT NOT NULL CHECK (vendor IN ('pulseforge', 'thermexwatch')),
    source_event_id  TEXT NOT NULL,
    machine_id       TEXT NOT NULL,
    plant_id         TEXT NOT NULL,
    line_id          TEXT NOT NULL,
    event_time       TIMESTAMPTZ NOT NULL,
    ingested_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    event_type       TEXT NOT NULL,
    attention_level  SMALLINT NOT NULL CHECK (attention_level BETWEEN 0 AND 4), -- 0=none .. 4=critical
    reason_code      TEXT NOT NULL DEFAULT '',
    vibration_value  DOUBLE PRECISION,
    vibration_unit   TEXT NOT NULL DEFAULT '',
    temperature_c    DOUBLE PRECISION,
    power_kw         DOUBLE PRECISION,
    sensor_health    DOUBLE PRECISION,
    machine_state    TEXT NOT NULL DEFAULT '',
    raw_payload      JSONB NOT NULL,

    CONSTRAINT ux_events_vendor_source_id UNIQUE (vendor, source_event_id),
    CONSTRAINT ux_events_content_key UNIQUE (vendor, machine_id, event_type, event_time)
);

-- Supports rules.Derive's fetch path: "every event seen for this machine".
CREATE INDEX idx_events_machine_id_event_time ON events (machine_id, event_time DESC);

-- ============================================================
-- batch_outcomes: per-record processing status within a batch.
-- One row per record slot (record_index = the record's position
-- in the vendor's original array), updated in place as the async
-- worker processes it. This is this project's dead-letter
-- representation: a row stuck at status='failed' after retries
-- is queryable via GET /v1/ingest/batches/{id}.
-- ============================================================
CREATE TABLE batch_outcomes (
    batch_id         TEXT NOT NULL REFERENCES batches (id) ON DELETE CASCADE,
    record_index     INT NOT NULL,
    source_event_id  TEXT NOT NULL DEFAULT '',
    vendor           TEXT NOT NULL CHECK (vendor IN ('pulseforge', 'thermexwatch')),
    status           TEXT NOT NULL DEFAULT 'queued'
                         CHECK (status IN ('queued', 'processed', 'duplicate', 'rejected', 'failed')),
    reason           TEXT NOT NULL DEFAULT '',
    attempts         INT NOT NULL DEFAULT 0,
    normalized_id    TEXT REFERENCES events (id),

    PRIMARY KEY (batch_id, record_index)
);

-- ============================================================
-- machine_views: cached derived operational view per machine,
-- recomputed by rules.Derive() in Go and upserted after every
-- event write. This is a read-optimized point-in-time snapshot,
-- not a history -- reason_codes/source_event_refs/latest_signals
-- are denormalized (array/JSONB) rather than joined out into
-- their own tables, since events already holds the full history.
-- ============================================================
CREATE TABLE machine_views (
    machine_id                 TEXT PRIMARY KEY,
    plant_id                   TEXT NOT NULL,
    line_id                    TEXT NOT NULL,
    derived_status              TEXT NOT NULL
                                    CHECK (derived_status IN ('normal', 'needs_attention', 'critical', 'under_maintenance', 'unknown')),
    needs_attention              BOOLEAN NOT NULL,
    attention_level              TEXT NOT NULL,
    reason_codes                 TEXT[] NOT NULL DEFAULT '{}',
    latest_relevant_event_time   TIMESTAMPTZ,
    processing_status            TEXT NOT NULL CHECK (processing_status IN ('fresh', 'stale', 'no_data')),
    last_processed_at            TIMESTAMPTZ,
    source_event_refs            JSONB NOT NULL DEFAULT '[]',
    latest_signals                JSONB NOT NULL DEFAULT '[]'
);
