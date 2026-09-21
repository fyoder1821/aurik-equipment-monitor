package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

// fakeSink is an EventSink test double that can be told to fail the first
// N calls to SaveEvent for a given source event id, to exercise the
// worker's retry/dead-letter path without needing a real store that can
// actually fail (the in-memory store never does).
type fakeSink struct {
	mu           sync.Mutex
	failFirstN   int
	callsBySrc   map[string]int
	saved        []domain.NormalizedEvent
	recomputed   []string
	forceDupOnce bool
	dupSeen      map[string]bool
}

func newFakeSink() *fakeSink {
	return &fakeSink{callsBySrc: map[string]int{}, dupSeen: map[string]bool{}}
}

func (f *fakeSink) SaveEvent(ctx context.Context, ev domain.NormalizedEvent) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.forceDupOnce {
		key := string(ev.Vendor) + "|" + ev.SourceEventID
		if f.dupSeen[key] {
			return true, nil
		}
		f.dupSeen[key] = true
	}

	f.callsBySrc[ev.SourceEventID]++
	if f.callsBySrc[ev.SourceEventID] <= f.failFirstN {
		return false, errors.New("simulated transient write failure")
	}
	f.saved = append(f.saved, ev)
	return false, nil
}

func (f *fakeSink) RecomputeMachine(ctx context.Context, machineID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recomputed = append(f.recomputed, machineID)
	return nil
}

// fakeBatchTracker records SetOutcome calls so tests can assert on them
// without a real store.Store.
type fakeBatchTracker struct {
	mu       sync.Mutex
	outcomes map[string]map[int]domain.RecordOutcome
}

func newFakeBatchTracker() *fakeBatchTracker {
	return &fakeBatchTracker{outcomes: map[string]map[int]domain.RecordOutcome{}}
}

func (f *fakeBatchTracker) SetOutcome(ctx context.Context, batchID string, index int, outcome domain.RecordOutcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.outcomes[batchID] == nil {
		f.outcomes[batchID] = map[int]domain.RecordOutcome{}
	}
	f.outcomes[batchID][index] = outcome
	return nil
}

func (f *fakeBatchTracker) get(batchID string, index int) (domain.RecordOutcome, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	o, ok := f.outcomes[batchID][index]
	return o, ok
}

func testRef(t *testing.T) *refdata.Store {
	t.Helper()
	ref, err := refdata.Load()
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return ref
}

func pulseForgeRaw(eventID, machineID string) json.RawMessage {
	return json.RawMessage(`{
		"event_id":"` + eventID + `","machine_id":"` + machineID + `","line_id":"LINE-A",
		"event_time":"2026-04-18T07:59:12Z","event_type":"HIGH_VIBRATION","severity":"high",
		"vibration_mm_s":11.8,"temperature_c":83.2
	}`)
}

func TestPool_RejectedRecord_NoRetryNoSave(t *testing.T) {
	sink := newFakeSink()
	tracker := newFakeBatchTracker()
	pool := NewPool(2, 8, sink, tracker, testRef(t))

	// empty machine_id -> normalize.PulseForge rejects it outright.
	pool.Submit(Job{BatchID: "b1", Index: 0, Vendor: domain.VendorPulseForge, Raw: pulseForgeRaw("PF-1", "")})
	pool.Close()

	outcome, ok := tracker.get("b1", 0)
	if !ok {
		t.Fatalf("no outcome recorded")
	}
	if outcome.Status != domain.StatusRejected {
		t.Errorf("status = %s, want rejected", outcome.Status)
	}
	if len(sink.saved) != 0 {
		t.Errorf("rejected record should never reach the sink, got %d saved", len(sink.saved))
	}
}

func TestPool_TransientFailure_RetriesThenSucceeds(t *testing.T) {
	sink := newFakeSink()
	sink.failFirstN = 2 // fails attempts 1 and 2, succeeds on attempt 3 (maxAttempts)
	tracker := newFakeBatchTracker()
	pool := NewPool(1, 8, sink, tracker, testRef(t))

	pool.Submit(Job{BatchID: "b1", Index: 0, Vendor: domain.VendorPulseForge, Raw: pulseForgeRaw("PF-1", "EQ-001")})
	pool.Close()

	outcome, ok := tracker.get("b1", 0)
	if !ok {
		t.Fatalf("no outcome recorded")
	}
	if outcome.Status != domain.StatusProcessed {
		t.Errorf("status = %s, want processed after retries succeed", outcome.Status)
	}
	if outcome.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", outcome.Attempts)
	}
	if len(sink.recomputed) != 1 {
		t.Errorf("expected RecomputeMachine to be called once, got %d calls", len(sink.recomputed))
	}
}

func TestPool_PersistentFailure_DeadLettered(t *testing.T) {
	sink := newFakeSink()
	sink.failFirstN = 999 // always fails
	tracker := newFakeBatchTracker()
	pool := NewPool(1, 8, sink, tracker, testRef(t))

	pool.Submit(Job{BatchID: "b1", Index: 0, Vendor: domain.VendorPulseForge, Raw: pulseForgeRaw("PF-1", "EQ-001")})
	pool.Close()

	outcome, ok := tracker.get("b1", 0)
	if !ok {
		t.Fatalf("no outcome recorded")
	}
	if outcome.Status != domain.StatusFailed {
		t.Errorf("status = %s, want failed (dead-lettered)", outcome.Status)
	}
	if outcome.Reason == "" {
		t.Errorf("expected a failure reason to be recorded")
	}
	if len(sink.recomputed) != 0 {
		t.Errorf("a dead-lettered record must not trigger recompute")
	}
}

func TestPool_DuplicateRecord_MarkedDuplicateNotReprocessed(t *testing.T) {
	sink := newFakeSink()
	sink.forceDupOnce = true
	tracker := newFakeBatchTracker()
	pool := NewPool(1, 8, sink, tracker, testRef(t))

	pool.Submit(Job{BatchID: "b1", Index: 0, Vendor: domain.VendorPulseForge, Raw: pulseForgeRaw("PF-1", "EQ-001")})
	pool.Submit(Job{BatchID: "b1", Index: 1, Vendor: domain.VendorPulseForge, Raw: pulseForgeRaw("PF-1", "EQ-001")})
	pool.Close()

	first, _ := tracker.get("b1", 0)
	second, _ := tracker.get("b1", 1)
	if first.Status != domain.StatusProcessed {
		t.Errorf("first record status = %s, want processed", first.Status)
	}
	if second.Status != domain.StatusDuplicate {
		t.Errorf("second (repeat) record status = %s, want duplicate", second.Status)
	}
	if len(sink.recomputed) != 1 {
		t.Errorf("duplicate must not trigger a second recompute, got %d calls", len(sink.recomputed))
	}
}
