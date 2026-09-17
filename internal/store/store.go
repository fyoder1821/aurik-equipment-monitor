// Package store is the in-memory, thread-safe system of record for this
// service: raw batches/outcomes, normalized events, and derived machine
// views. A real deployment would back this with a database (see the
// README for that trade-off); every method here is written against a
// small enough surface that swapping the implementation later is a
// contained change.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
	"aurik-equipment-monitor/internal/rules"
)

type Store struct {
	mu sync.RWMutex

	ref *refdata.Store

	batches map[string]*domain.Batch
	events  map[string][]domain.NormalizedEvent // by machine_id
	views   map[string]domain.MachineView       // by machine_id

	seenIDs     map[string]bool // vendor|source_event_id
	seenContent map[string]bool // vendor|machine_id|event_type|event_time
}

func New(ref *refdata.Store) *Store {
	return &Store{
		ref:         ref,
		batches:     make(map[string]*domain.Batch),
		events:      make(map[string][]domain.NormalizedEvent),
		views:       make(map[string]domain.MachineView),
		seenIDs:     make(map[string]bool),
		seenContent: make(map[string]bool),
	}
}

func randID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

// CreateBatch registers a new ingestion batch with `recordCount` pending
// outcome slots, and returns it.
func (s *Store) CreateBatch(vendor domain.Vendor, recordCount int) *domain.Batch {
	s.mu.Lock()
	defer s.mu.Unlock()

	b := &domain.Batch{
		ID:          randID("batch"),
		Vendor:      vendor,
		ReceivedAt:  time.Now().UTC(),
		RecordCount: recordCount,
		Outcomes:    make([]domain.RecordOutcome, recordCount),
	}
	s.batches[b.ID] = b
	return b
}

// SetOutcome records the processing outcome for one record within a batch.
func (s *Store) SetOutcome(batchID string, index int, outcome domain.RecordOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.batches[batchID]
	if !ok || index < 0 || index >= len(b.Outcomes) {
		return
	}
	b.Outcomes[index] = outcome
}

// GetBatch returns a snapshot of a batch's status.
func (s *Store) GetBatch(id string) (domain.Batch, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	b, ok := s.batches[id]
	if !ok {
		return domain.Batch{}, false
	}
	cp := *b
	cp.Outcomes = append([]domain.RecordOutcome(nil), b.Outcomes...)
	return cp, true
}

// SaveEvent stores a normalized event if it isn't a duplicate of one
// already seen, checked both by the vendor's own record id and by a
// content key (vendor+machine+event_type+event_time) that catches a
// vendor retry sent under a new record id. It reports whether the event
// was a duplicate; a nil error always -- see worker.EventSink for why the
// signature still allows for one.
func (s *Store) SaveEvent(ev domain.NormalizedEvent) (duplicate bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	idKey := string(ev.Vendor) + "|" + ev.SourceEventID
	contentKey := fmt.Sprintf("%s|%s|%s|%s", ev.Vendor, ev.MachineID, ev.EventType, ev.EventTime.UTC().Format(time.RFC3339))

	if s.seenIDs[idKey] || s.seenContent[contentKey] {
		return true, nil
	}
	s.seenIDs[idKey] = true
	s.seenContent[contentKey] = true
	s.events[ev.MachineID] = append(s.events[ev.MachineID], ev)
	return false, nil
}

// RecomputeMachine re-derives and caches the operational view for one
// machine from everything stored for it so far.
func (s *Store) RecomputeMachine(machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	asset, ok := s.ref.Machine(machineID)
	if !ok {
		return
	}
	events := append([]domain.NormalizedEvent(nil), s.events[machineID]...)
	s.views[machineID] = rules.Derive(machineID, events, asset, time.Now().UTC())
}

// GetMachineView returns the derived view for a known machine. The second
// return value is false only if the machine id isn't in the asset
// reference at all (never simply "no data yet" -- that's ProcessingStatus
// "no_data" on a normal view).
func (s *Store) GetMachineView(machineID string) (domain.MachineView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	asset, ok := s.ref.Machine(machineID)
	if !ok {
		return domain.MachineView{}, false
	}
	if view, ok := s.views[machineID]; ok {
		return view, true
	}
	return emptyView(asset), true
}

// ListMachineViews returns every known machine's view, optionally filtered
// by plant id and/or derived status, sorted by machine id.
func (s *Store) ListMachineViews(plantFilter, statusFilter string) []domain.MachineView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []domain.MachineView
	for _, a := range s.ref.AllMachines() {
		if plantFilter != "" && a.PlantID != plantFilter {
			continue
		}
		view, ok := s.views[a.MachineID]
		if !ok {
			view = emptyView(a)
		}
		if statusFilter != "" && string(view.DerivedStatus) != statusFilter {
			continue
		}
		out = append(out, view)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MachineID < out[j].MachineID })
	return out
}

// PlantSummary rolls up per-machine derived status for one plant.
func (s *Store) PlantSummary(plantID string) (domain.PlantSummary, error) {
	machines := s.ref.MachinesByPlant(plantID)
	if len(machines) == 0 {
		return domain.PlantSummary{}, fmt.Errorf("unknown plant_id %q", plantID)
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	summary := domain.PlantSummary{
		PlantID:       plantID,
		TotalMachines: len(machines),
		CountByStatus: map[string]int{},
	}
	needsAttentionByLine := map[string]int{}

	for _, m := range machines {
		view, ok := s.views[m.MachineID]
		if !ok {
			view = emptyView(m)
		}
		summary.CountByStatus[string(view.DerivedStatus)]++
		if view.DerivedStatus == domain.DerivedCritical {
			summary.CriticalMachines = append(summary.CriticalMachines, m.MachineID)
		}
		if view.ProcessingStatus == "stale" || view.ProcessingStatus == "no_data" {
			summary.StaleMachines = append(summary.StaleMachines, m.MachineID)
		}
		if view.NeedsAttention {
			needsAttentionByLine[m.LineID]++
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
