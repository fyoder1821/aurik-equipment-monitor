// Package worker is the async processing layer: a small in-process job
// queue with a fixed pool of goroutines. Ingestion handlers enqueue a job
// per raw record and return immediately; normalization, dedupe, storage,
// and derived-status recomputation all happen off the request path here.
//
// Two failure classes are handled differently, on purpose:
//   - Normalization rejects (bad/unresolvable input) are permanent -- the
//     job is not retried, it's recorded as "rejected" with a reason.
//   - Sink errors (a failed write) are treated as potentially transient --
//     the job is retried with backoff up to maxAttempts, then recorded as
//     "failed" (this service's dead-letter representation, queryable via
//     the batch status endpoint). The in-memory store used in this project
//     never actually errors on write, so this path only fires against a
//     real (e.g. DB-backed) sink or a fault-injecting test double -- see
//     worker_test.go.
package worker

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/normalize"
	"aurik-equipment-monitor/internal/refdata"
)

// EventSink is the storage dependency the worker needs. store.Store
// satisfies it; tests use a fake to exercise retry/dead-letter behavior.
type EventSink interface {
	SaveEvent(ev domain.NormalizedEvent) (duplicate bool, err error)
	RecomputeMachine(machineID string)
}

// BatchTracker records per-record outcomes back onto a batch.
type BatchTracker interface {
	SetOutcome(batchID string, index int, outcome domain.RecordOutcome)
}

// Job is one raw vendor record queued for async normalization.
type Job struct {
	BatchID string
	Index   int
	Vendor  domain.Vendor
	Raw     json.RawMessage
}

const defaultMaxAttempts = 3

// Pool is a fixed-size worker pool draining a buffered job channel.
type Pool struct {
	jobs        chan Job
	sink        EventSink
	batches     BatchTracker
	ref         *refdata.Store
	maxAttempts int
	wg          sync.WaitGroup
}

func NewPool(workers, queueSize int, sink EventSink, batches BatchTracker, ref *refdata.Store) *Pool {
	p := &Pool{
		jobs:        make(chan Job, queueSize),
		sink:        sink,
		batches:     batches,
		ref:         ref,
		maxAttempts: defaultMaxAttempts,
	}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go p.loop()
	}
	return p
}

// Submit enqueues a job. It blocks if the queue is full, which is a
// deliberate (if simple) form of backpressure -- see the README for the
// production follow-up (bounded submit with a 503 instead of blocking).
func (p *Pool) Submit(j Job) {
	p.jobs <- j
}

// Close stops accepting new work and waits for in-flight jobs to drain.
// Mainly used by tests that need processing to have finished before
// asserting on outcomes.
func (p *Pool) Close() {
	close(p.jobs)
	p.wg.Wait()
}

func (p *Pool) loop() {
	defer p.wg.Done()
	for j := range p.jobs {
		p.process(j)
	}
}

func (p *Pool) process(j Job) {
	var outcome normalize.Outcome
	switch j.Vendor {
	case domain.VendorPulseForge:
		outcome = normalize.PulseForge(j.Raw, p.ref)
	case domain.VendorThermexWatch:
		outcome = normalize.ThermexWatch(j.Raw, p.ref)
	default:
		p.batches.SetOutcome(j.BatchID, j.Index, domain.RecordOutcome{
			Vendor: j.Vendor,
			Status: domain.StatusRejected,
			Reason: fmt.Sprintf("unsupported vendor %q", j.Vendor),
		})
		return
	}

	if outcome.Reject != "" {
		p.batches.SetOutcome(j.BatchID, j.Index, domain.RecordOutcome{
			Vendor: j.Vendor,
			Status: domain.StatusRejected,
			Reason: outcome.Reject,
		})
		return
	}

	ev := outcome.Event
	var lastErr error
	for attempt := 1; attempt <= p.maxAttempts; attempt++ {
		dup, err := p.sink.SaveEvent(ev)
		if err == nil {
			if dup {
				p.batches.SetOutcome(j.BatchID, j.Index, domain.RecordOutcome{
					SourceEventID: ev.SourceEventID,
					Vendor:        j.Vendor,
					Status:        domain.StatusDuplicate,
					Reason:        "matches a previously processed record id or content",
					Attempts:      attempt,
				})
				return
			}
			p.sink.RecomputeMachine(ev.MachineID)
			p.batches.SetOutcome(j.BatchID, j.Index, domain.RecordOutcome{
				SourceEventID: ev.SourceEventID,
				Vendor:        j.Vendor,
				Status:        domain.StatusProcessed,
				Attempts:      attempt,
				NormalizedID:  ev.ID,
			})
			return
		}
		lastErr = err
		time.Sleep(time.Duration(attempt) * 20 * time.Millisecond)
	}

	p.batches.SetOutcome(j.BatchID, j.Index, domain.RecordOutcome{
		SourceEventID: ev.SourceEventID,
		Vendor:        j.Vendor,
		Status:        domain.StatusFailed,
		Reason:        fmt.Sprintf("processing failed after %d attempts: %v", p.maxAttempts, lastErr),
		Attempts:      p.maxAttempts,
	})
}
