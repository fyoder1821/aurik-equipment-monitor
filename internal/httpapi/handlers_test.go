package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"aurik-equipment-monitor/internal/dbtest"
	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
	"aurik-equipment-monitor/internal/store"
	"aurik-equipment-monitor/internal/worker"
)

var testDSN string

// TestMain starts one Postgres container for every test in this package
// and tears it down once, rather than per-test -- see internal/dbtest.
func TestMain(m *testing.M) {
	dsn, cleanup, err := dbtest.StartContainer()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	testDSN = dsn
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// newTestServer wires the real store (against an ephemeral Postgres
// container, not a fake) and worker pool behind the router, so these
// tests cover the full ingest -> async normalize -> derive -> serve path
// exactly as it runs in production.
func newTestServer(t *testing.T) (*httptest.Server, *worker.Pool) {
	t.Helper()
	ref, err := refdata.Load()
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	pool := dbtest.Connect(t, testDSN)
	st := store.New(pool, ref)
	workerPool := worker.NewPool(2, 32, st, st, ref)
	srv := httptest.NewServer(NewRouter(st, workerPool))
	t.Cleanup(srv.Close)
	return srv, workerPool
}

// drain closes the pool so all queued jobs finish before assertions run --
// avoids sleeps/polling for the async work to land.
func drain(pool *worker.Pool) {
	pool.Close()
}

func TestIngestAndMachineView_HappyPath(t *testing.T) {
	srv, pool := newTestServer(t)

	body := `{
		"vendor":"PulseForge","plant_id":"PLANT_01","batch_generated_at":"2026-04-18T08:05:00Z",
		"events":[{
			"event_id":"PF-1001","machine_id":"EQ-001","line_id":"LINE-A",
			"event_time":"2026-04-18T07:59:12Z","event_type":"HIGH_VIBRATION","severity":"high",
			"vibration_mm_s":11.8,"temperature_c":83.2,"machine_state":"running",
			"sensor_health":0.91,"vendor_confidence":0.87
		}]
	}`
	resp, err := http.Post(srv.URL+"/v1/ingest/pulseforge", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST ingest: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	var ingestResp struct {
		BatchID string `json:"batch_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ingestResp); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	if ingestResp.BatchID == "" {
		t.Fatalf("expected a non-empty batch_id")
	}

	drain(pool)

	// Batch status should show the record processed.
	batchResp, err := http.Get(srv.URL + "/v1/ingest/batches/" + ingestResp.BatchID)
	if err != nil {
		t.Fatalf("GET batch status: %v", err)
	}
	defer batchResp.Body.Close()
	var batch domain.Batch
	if err := json.NewDecoder(batchResp.Body).Decode(&batch); err != nil {
		t.Fatalf("decode batch: %v", err)
	}
	if len(batch.Outcomes) != 1 || batch.Outcomes[0].Status != domain.StatusProcessed {
		t.Fatalf("batch outcomes = %+v, want one processed record", batch.Outcomes)
	}

	// Machine view should reflect the ingested event.
	viewResp, err := http.Get(srv.URL + "/v1/machines/EQ-001")
	if err != nil {
		t.Fatalf("GET machine view: %v", err)
	}
	defer viewResp.Body.Close()
	var view domain.MachineView
	if err := json.NewDecoder(viewResp.Body).Decode(&view); err != nil {
		t.Fatalf("decode machine view: %v", err)
	}
	if view.DerivedStatus != domain.DerivedNeedsAttention {
		t.Errorf("derived_status = %s, want needs_attention", view.DerivedStatus)
	}
	if !view.NeedsAttention {
		t.Errorf("needs_attention = false, want true")
	}
}

func TestIngest_MalformedEnvelope_Returns400(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Post(srv.URL+"/v1/ingest/pulseforge", "application/json", strings.NewReader(`not json`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestIngest_OneBadRecordDoesNotSinkBatch(t *testing.T) {
	srv, pool := newTestServer(t)

	// Mirrors vendor_api_samples/malformed.json: one record with an empty
	// machine_id, one healthy record. Only the bad one should be rejected.
	body := `{
		"vendor":"PulseForge","plant_id":"PLANT_01","batch_generated_at":"2026-04-18T08:30:00Z",
		"events":[
			{"event_id":"PF-1201","machine_id":"","line_id":"LINE-A","event_time":"2026-04-18T08:28:12Z","event_type":"HIGH_VIBRATION","severity":"high","vibration_mm_s":10.9,"temperature_c":82.5},
			{"event_id":"PF-1205","machine_id":"EQ-002","line_id":"LINE-A","event_time":"2026-04-18T08:28:20Z","event_type":"RECOVERY_SIGNAL","severity":"low","vibration_mm_s":3.0,"temperature_c":65.0}
		]
	}`
	resp, err := http.Post(srv.URL+"/v1/ingest/pulseforge", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (batch accepted even though one record is bad)", resp.StatusCode)
	}
	var ingestResp struct {
		BatchID string `json:"batch_id"`
	}
	json.NewDecoder(resp.Body).Decode(&ingestResp)

	drain(pool)

	batchResp, err := http.Get(srv.URL + "/v1/ingest/batches/" + ingestResp.BatchID)
	if err != nil {
		t.Fatalf("GET batch status: %v", err)
	}
	defer batchResp.Body.Close()
	var batch domain.Batch
	json.NewDecoder(batchResp.Body).Decode(&batch)

	if len(batch.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(batch.Outcomes))
	}
	if batch.Outcomes[0].Status != domain.StatusRejected {
		t.Errorf("record 0 status = %s, want rejected", batch.Outcomes[0].Status)
	}
	if batch.Outcomes[1].Status != domain.StatusProcessed {
		t.Errorf("record 1 status = %s, want processed", batch.Outcomes[1].Status)
	}
}

func TestPlantSummary_Endpoint(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Get(srv.URL + "/v1/plants/PLANT_01/summary")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var summary domain.PlantSummary
	if err := json.NewDecoder(resp.Body).Decode(&summary); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if summary.TotalMachines == 0 {
		t.Errorf("expected a non-zero machine count for PLANT_01")
	}
}

func TestPlantSummary_UnknownPlant_Returns404(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Get(srv.URL + "/v1/plants/PLANT_99/summary")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestMachineView_UnknownMachine_Returns404(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Get(srv.URL + "/v1/machines/EQ-999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestBatchStatus_UnknownBatch_Returns404(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Get(srv.URL + "/v1/ingest/batches/does-not-exist")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	srv, pool := newTestServer(t)
	defer drain(pool)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}
