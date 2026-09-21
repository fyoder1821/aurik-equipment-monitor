// Package store is the PostgreSQL-backed system of record for this
// service: raw batches/outcomes, normalized events, and derived machine
// views. See db/schema.sql for the table definitions this package assumes
// already exist -- this package does not run migrations itself.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
	"aurik-equipment-monitor/internal/rules"
)

const uniqueViolation = "23505"

type Store struct {
	pool *pgxpool.Pool
	ref  *refdata.Store
}

func New(pool *pgxpool.Pool, ref *refdata.Store) *Store {
	return &Store{pool: pool, ref: ref}
}

func randID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// CreateBatch registers a new ingestion batch and pre-creates one
// batch_outcomes row per record slot (status defaults to 'queued'), so a
// status check that lands before the worker has processed anything still
// sees an accurate "queued" rather than a missing row.
func (s *Store) CreateBatch(ctx context.Context, vendor domain.Vendor, recordCount int) (*domain.Batch, error) {
	id := randID("batch")
	receivedAt := time.Now().UTC()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once committed

	if _, err := tx.Exec(ctx,
		`INSERT INTO batches (id, vendor, received_at, record_count) VALUES ($1, $2, $3, $4)`,
		id, string(vendor), receivedAt, recordCount,
	); err != nil {
		return nil, fmt.Errorf("store: insert batch: %w", err)
	}

	if recordCount > 0 {
		if _, err := tx.Exec(ctx,
			`INSERT INTO batch_outcomes (batch_id, record_index, vendor)
			 SELECT $1, generate_series(0, $2::int - 1), $3`,
			id, recordCount, string(vendor),
		); err != nil {
			return nil, fmt.Errorf("store: insert batch outcomes: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit: %w", err)
	}

	return &domain.Batch{ID: id, Vendor: vendor, ReceivedAt: receivedAt, RecordCount: recordCount}, nil
}

// SetOutcome records the processing outcome for one record within a batch.
func (s *Store) SetOutcome(ctx context.Context, batchID string, index int, outcome domain.RecordOutcome) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE batch_outcomes
		SET source_event_id = $3, status = $4, reason = $5, attempts = $6, normalized_id = NULLIF($7, '')
		WHERE batch_id = $1 AND record_index = $2
	`, batchID, index, outcome.SourceEventID, string(outcome.Status), outcome.Reason, outcome.Attempts, outcome.NormalizedID)
	if err != nil {
		return fmt.Errorf("store: set outcome: %w", err)
	}
	return nil
}

// GetBatch returns a batch and its per-record outcomes.
func (s *Store) GetBatch(ctx context.Context, id string) (domain.Batch, bool, error) {
	var b domain.Batch
	var vendor string
	err := s.pool.QueryRow(ctx,
		`SELECT id, vendor, received_at, record_count FROM batches WHERE id = $1`, id,
	).Scan(&b.ID, &vendor, &b.ReceivedAt, &b.RecordCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Batch{}, false, nil
	}
	if err != nil {
		return domain.Batch{}, false, fmt.Errorf("store: get batch: %w", err)
	}
	b.Vendor = domain.Vendor(vendor)

	rows, err := s.pool.Query(ctx, `
		SELECT record_index, source_event_id, vendor, status, reason, attempts, COALESCE(normalized_id, '')
		FROM batch_outcomes WHERE batch_id = $1 ORDER BY record_index
	`, id)
	if err != nil {
		return domain.Batch{}, false, fmt.Errorf("store: get batch outcomes: %w", err)
	}
	defer rows.Close()

	b.Outcomes = make([]domain.RecordOutcome, b.RecordCount)
	for rows.Next() {
		var idx int
		var o domain.RecordOutcome
		var vendorStr, statusStr string
		if err := rows.Scan(&idx, &o.SourceEventID, &vendorStr, &statusStr, &o.Reason, &o.Attempts, &o.NormalizedID); err != nil {
			return domain.Batch{}, false, fmt.Errorf("store: scan outcome: %w", err)
		}
		o.Vendor = domain.Vendor(vendorStr)
		o.Status = domain.RecordStatus(statusStr)
		if idx >= 0 && idx < len(b.Outcomes) {
			b.Outcomes[idx] = o
		}
	}
	if err := rows.Err(); err != nil {
		return domain.Batch{}, false, fmt.Errorf("store: iterate outcomes: %w", err)
	}
	return b, true, nil
}

// SaveEvent inserts a normalized event. Idempotency is enforced by the
// database, not application code: events has two UNIQUE constraints (the
// vendor's own record id, and a vendor+machine+event_type+event_time
// content key that catches a vendor retry sent under a new record id).
// A unique-violation on either is reported as a duplicate, not an error.
func (s *Store) SaveEvent(ctx context.Context, ev domain.NormalizedEvent) (duplicate bool, err error) {
	_, err = s.pool.Exec(ctx, `
		INSERT INTO events (
			id, vendor, source_event_id, machine_id, plant_id, line_id, event_time,
			event_type, attention_level, reason_code, vibration_value, vibration_unit,
			temperature_c, power_kw, sensor_health, machine_state, raw_payload
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`,
		ev.ID, string(ev.Vendor), ev.SourceEventID, ev.MachineID, ev.PlantID, ev.LineID, ev.EventTime,
		ev.EventType, int(ev.AttentionLevel), ev.ReasonCode, ev.VibrationValue, ev.VibrationUnit,
		ev.TemperatureC, ev.PowerKW, ev.SensorHealth, ev.MachineState, string(ev.RawPayload),
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return true, nil
		}
		return false, fmt.Errorf("store: save event: %w", err)
	}
	return false, nil
}

// RecomputeMachine re-derives and upserts the cached operational view for
// one machine from every event stored for it so far.
func (s *Store) RecomputeMachine(ctx context.Context, machineID string) error {
	asset, ok := s.ref.Machine(machineID)
	if !ok {
		return nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT vendor, source_event_id, machine_id, plant_id, line_id, event_time, event_type,
		       attention_level, reason_code, vibration_value, vibration_unit, temperature_c,
		       power_kw, sensor_health, machine_state
		FROM events WHERE machine_id = $1
	`, machineID)
	if err != nil {
		return fmt.Errorf("store: fetch events for %s: %w", machineID, err)
	}
	defer rows.Close()

	var events []domain.NormalizedEvent
	for rows.Next() {
		var e domain.NormalizedEvent
		var vendorStr string
		var attn int
		if err := rows.Scan(&vendorStr, &e.SourceEventID, &e.MachineID, &e.PlantID, &e.LineID, &e.EventTime,
			&e.EventType, &attn, &e.ReasonCode, &e.VibrationValue, &e.VibrationUnit, &e.TemperatureC,
			&e.PowerKW, &e.SensorHealth, &e.MachineState); err != nil {
			return fmt.Errorf("store: scan event: %w", err)
		}
		e.Vendor = domain.Vendor(vendorStr)
		e.AttentionLevel = domain.AttentionLevel(attn)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate events: %w", err)
	}

	view := rules.Derive(machineID, events, asset, time.Now().UTC())
	return s.upsertView(ctx, view)
}

func (s *Store) upsertView(ctx context.Context, v domain.MachineView) error {
	sourceRefs, err := json.Marshal(v.SourceEventRefs)
	if err != nil {
		return fmt.Errorf("store: marshal source_event_refs: %w", err)
	}
	signals, err := json.Marshal(v.LatestSignals)
	if err != nil {
		return fmt.Errorf("store: marshal latest_signals: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO machine_views (
			machine_id, plant_id, line_id, derived_status, needs_attention, attention_level,
			reason_codes, latest_relevant_event_time, processing_status, last_processed_at,
			source_event_refs, latest_signals
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (machine_id) DO UPDATE SET
			plant_id = EXCLUDED.plant_id,
			line_id = EXCLUDED.line_id,
			derived_status = EXCLUDED.derived_status,
			needs_attention = EXCLUDED.needs_attention,
			attention_level = EXCLUDED.attention_level,
			reason_codes = EXCLUDED.reason_codes,
			latest_relevant_event_time = EXCLUDED.latest_relevant_event_time,
			processing_status = EXCLUDED.processing_status,
			last_processed_at = EXCLUDED.last_processed_at,
			source_event_refs = EXCLUDED.source_event_refs,
			latest_signals = EXCLUDED.latest_signals
	`,
		v.MachineID, v.PlantID, v.LineID, string(v.DerivedStatus), v.NeedsAttention, v.AttentionLevel,
		v.ReasonCodes, v.LatestRelevantEventTime, v.ProcessingStatus, v.LastProcessedAt,
		string(sourceRefs), string(signals),
	)
	if err != nil {
		return fmt.Errorf("store: upsert machine_view: %w", err)
	}
	return nil
}

// rowScanner is satisfied by both pgx.Row (QueryRow) and pgx.Rows (Query),
// so GetMachineView and the batch fetch path below can share one scan.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanView(row rowScanner) (domain.MachineView, error) {
	var v domain.MachineView
	var derivedStatus string
	var sourceRefsJSON, signalsJSON []byte

	if err := row.Scan(&v.MachineID, &v.PlantID, &v.LineID, &derivedStatus, &v.NeedsAttention, &v.AttentionLevel,
		&v.ReasonCodes, &v.LatestRelevantEventTime, &v.ProcessingStatus, &v.LastProcessedAt,
		&sourceRefsJSON, &signalsJSON); err != nil {
		return domain.MachineView{}, err
	}
	v.DerivedStatus = domain.DerivedStatus(derivedStatus)
	if err := json.Unmarshal(sourceRefsJSON, &v.SourceEventRefs); err != nil {
		return domain.MachineView{}, fmt.Errorf("unmarshal source_event_refs: %w", err)
	}
	if err := json.Unmarshal(signalsJSON, &v.LatestSignals); err != nil {
		return domain.MachineView{}, fmt.Errorf("unmarshal latest_signals: %w", err)
	}
	if v.ReasonCodes == nil {
		v.ReasonCodes = []string{}
	}
	if v.SourceEventRefs == nil {
		v.SourceEventRefs = []domain.EventRef{}
	}
	if v.LatestSignals == nil {
		v.LatestSignals = []domain.SignalSnapshot{}
	}
	return v, nil
}

// GetMachineView returns the derived view for a known machine. The second
// return value is false only if the machine id isn't in the asset
// reference at all (never simply "no data yet" -- that's ProcessingStatus
// "no_data" on a normal view).
func (s *Store) GetMachineView(ctx context.Context, machineID string) (domain.MachineView, bool, error) {
	asset, ok := s.ref.Machine(machineID)
	if !ok {
		return domain.MachineView{}, false, nil
	}

	row := s.pool.QueryRow(ctx, `
		SELECT machine_id, plant_id, line_id, derived_status, needs_attention, attention_level,
		       reason_codes, latest_relevant_event_time, processing_status, last_processed_at,
		       source_event_refs, latest_signals
		FROM machine_views WHERE machine_id = $1
	`, machineID)
	view, err := scanView(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyView(asset), true, nil
	}
	if err != nil {
		return domain.MachineView{}, false, fmt.Errorf("store: get machine view: %w", err)
	}
	return view, true, nil
}

// fetchViews batch-fetches cached views for a set of machine ids in one
// round trip, keyed by machine id, so ListMachineViews/PlantSummary avoid
// an N+1 query pattern.
func (s *Store) fetchViews(ctx context.Context, machineIDs []string) (map[string]domain.MachineView, error) {
	out := make(map[string]domain.MachineView, len(machineIDs))
	if len(machineIDs) == 0 {
		return out, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT machine_id, plant_id, line_id, derived_status, needs_attention, attention_level,
		       reason_codes, latest_relevant_event_time, processing_status, last_processed_at,
		       source_event_refs, latest_signals
		FROM machine_views WHERE machine_id = ANY($1)
	`, machineIDs)
	if err != nil {
		return nil, fmt.Errorf("store: fetch views: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan view: %w", err)
		}
		out[v.MachineID] = v
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate views: %w", err)
	}
	return out, nil
}

// ListMachineViews returns every known machine's view, optionally filtered
// by plant id and/or derived status, sorted by machine id.
func (s *Store) ListMachineViews(ctx context.Context, plantFilter, statusFilter string) ([]domain.MachineView, error) {
	var machines []refdata.Asset
	if plantFilter != "" {
		machines = s.ref.MachinesByPlant(plantFilter)
	} else {
		machines = s.ref.AllMachines()
	}

	ids := make([]string, len(machines))
	assetByID := make(map[string]refdata.Asset, len(machines))
	for i, a := range machines {
		ids[i] = a.MachineID
		assetByID[a.MachineID] = a
	}

	views, err := s.fetchViews(ctx, ids)
	if err != nil {
		return nil, err
	}

	out := make([]domain.MachineView, 0, len(machines))
	for _, id := range ids {
		view, ok := views[id]
		if !ok {
			view = emptyView(assetByID[id])
		}
		if statusFilter != "" && string(view.DerivedStatus) != statusFilter {
			continue
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
	return out, nil
}

// PlantSummary rolls up per-machine derived status for one plant.
func (s *Store) PlantSummary(ctx context.Context, plantID string) (domain.PlantSummary, error) {
	machines := s.ref.MachinesByPlant(plantID)
	if len(machines) == 0 {
		return domain.PlantSummary{}, fmt.Errorf("%w: unknown plant_id %q", domain.ErrNotFound, plantID)
	}

	ids := make([]string, len(machines))
	assetByID := make(map[string]refdata.Asset, len(machines))
	for i, a := range machines {
		ids[i] = a.MachineID
		assetByID[a.MachineID] = a
	}

	views, err := s.fetchViews(ctx, ids)
	if err != nil {
		return domain.PlantSummary{}, err
	}

	summary := domain.PlantSummary{
		PlantID:       plantID,
		TotalMachines: len(machines),
		CountByStatus: map[string]int{},
	}
	needsAttentionByLine := map[string]int{}

	for _, id := range ids {
		view, ok := views[id]
		if !ok {
			view = emptyView(assetByID[id])
		}
		summary.CountByStatus[string(view.DerivedStatus)]++
		if view.DerivedStatus == domain.DerivedCritical {
			summary.CriticalMachines = append(summary.CriticalMachines, id)
		}
		if view.ProcessingStatus == "stale" || view.ProcessingStatus == "no_data" {
			summary.StaleMachines = append(summary.StaleMachines, id)
		}
		if view.NeedsAttention {
			needsAttentionByLine[assetByID[id].LineID]++
		}
	}

	for lineID, count := range needsAttentionByLine {
		line, _ := s.ref.Line(plantID, lineID)
		summary.Lines = append(summary.Lines, domain.LineSummary{
			LineID:                   lineID,
			LineName:                 line.LineName,
			MachinesNeedingAttention: count,
		})
	}
	sort.Slice(summary.Lines, func(i, j int) bool { return summary.Lines[i].LineID < summary.Lines[j].LineID })
	sort.Strings(summary.CriticalMachines)
	sort.Strings(summary.StaleMachines)

	return summary, nil
}

func emptyView(a refdata.Asset) domain.MachineView {
	return domain.MachineView{
		MachineID:        a.MachineID,
		PlantID:          a.PlantID,
		LineID:           a.LineID,
		DerivedStatus:    domain.DerivedUnknown,
		AttentionLevel:   domain.AttentionNone.String(),
		ProcessingStatus: "no_data",
		ReasonCodes:      []string{},
		SourceEventRefs:  []domain.EventRef{},
		LatestSignals:    []domain.SignalSnapshot{},
	}
}
