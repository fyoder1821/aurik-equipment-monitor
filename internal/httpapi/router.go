// Package httpapi wires the ingestion and output-serving HTTP endpoints.
package httpapi

import (
	"log"
	"net/http"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/worker"
)

// Store is the read/write surface httpapi needs from the in-memory store.
// Kept as an interface so handler tests can use a fake instead of standing
// up a real store.Store.
type Store interface {
	CreateBatch(vendor domain.Vendor, recordCount int) *domain.Batch
	GetBatch(id string) (domain.Batch, bool)
	GetMachineView(machineID string) (domain.MachineView, bool)
	ListMachineViews(plantFilter, statusFilter string) []domain.MachineView
	PlantSummary(plantID string) (domain.PlantSummary, error)
}

// Pool is the async submission surface httpapi needs from worker.Pool.
type Pool interface {
	Submit(j worker.Job)
}

type api struct {
	store Store
	pool  Pool
}

// NewRouter builds the full HTTP handler for the service.
func NewRouter(store Store, pool Pool) http.Handler {
	a := &api{store: store, pool: pool}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.handleHealth)
	mux.HandleFunc("POST /v1/ingest/pulseforge", a.handleIngest(domain.VendorPulseForge))
	mux.HandleFunc("POST /v1/ingest/thermexwatch", a.handleIngest(domain.VendorThermexWatch))
	mux.HandleFunc("GET /v1/ingest/batches/{batchID}", a.handleGetBatch)
	mux.HandleFunc("GET /v1/machines", a.handleListMachines)
	mux.HandleFunc("GET /v1/machines/{machineID}", a.handleGetMachine)
	mux.HandleFunc("GET /v1/plants/{plantID}/summary", a.handlePlantSummary)

	return withLogging(mux)
}

func (a *api) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s -> %d (%s)", r.Method, r.URL.Path, rec.status, time.Since(start))
	})
}
