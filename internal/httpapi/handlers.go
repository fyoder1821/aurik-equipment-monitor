package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/worker"
)

// maxBodyBytes bounds request size -- cheap protection against an
// accidentally (or maliciously) huge payload before it's even parsed.
const maxBodyBytes = 1 << 20 // 1MB

// handleIngest accepts a single-vendor batch envelope, splits it into
// per-record raw JSON, and hands each record to the async worker pool.
// The HTTP response only reflects whether the *envelope* was well-formed;
// per-record outcomes are only known after async processing and are
// fetched via the batch status endpoint. This split is what makes
// ingestion resilient to one bad record in an otherwise-valid batch.
func (a *api) handleIngest(vendor domain.Vendor) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "cannot read request body")
			return
		}
		if len(body) > maxBodyBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}

		records, err := extractRecords(vendor, body)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("malformed request envelope: %v", err))
			return
		}
		if len(records) == 0 {
			writeError(w, http.StatusBadRequest, "payload contains no records")
			return
		}

		batch, err := a.store.CreateBatch(r.Context(), vendor, len(records))
		if err != nil {
			log.Printf("create batch: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to accept batch")
			return
		}
		for i, raw := range records {
			a.pool.Submit(worker.Job{BatchID: batch.ID, Index: i, Vendor: vendor, Raw: raw})
		}

		writeJSON(w, http.StatusAccepted, map[string]any{
			"batch_id":     batch.ID,
			"vendor":       vendor,
			"record_count": len(records),
			"status":       "queued",
			"status_url":   "/v1/ingest/batches/" + batch.ID,
		})
	}
}

// extractRecords pulls the vendor-specific array field (PulseForge's
// "events", ThermexWatch's "readings") out of the envelope as raw JSON, so
// each record can be normalized independently downstream.
func extractRecords(vendor domain.Vendor, body []byte) ([]json.RawMessage, error) {
	switch vendor {
	case domain.VendorPulseForge:
		var env struct {
			Events []json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		return env.Events, nil
	case domain.VendorThermexWatch:
		var env struct {
			Readings []json.RawMessage `json:"readings"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return nil, err
		}
		return env.Readings, nil
	default:
		return nil, fmt.Errorf("unsupported vendor %q", vendor)
	}
}

func (a *api) handleGetBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("batchID")
	b, ok, err := a.store.GetBatch(r.Context(), id)
	if err != nil {
		log.Printf("get batch %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to load batch")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "batch not found")
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (a *api) handleGetMachine(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("machineID")
	view, ok, err := a.store.GetMachineView(r.Context(), id)
	if err != nil {
		log.Printf("get machine view %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to load machine view")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "unknown machine_id")
		return
	}
	writeJSON(w, http.StatusOK, view)
}

func (a *api) handleListMachines(w http.ResponseWriter, r *http.Request) {
	plantFilter := r.URL.Query().Get("plant_id")
	statusFilter := r.URL.Query().Get("status")
	views, err := a.store.ListMachineViews(r.Context(), plantFilter, statusFilter)
	if err != nil {
		log.Printf("list machines: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list machines")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machines": views,
		"count":    len(views),
	})
}

func (a *api) handlePlantSummary(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("plantID")
	summary, err := a.store.PlantSummary(r.Context(), id)
	if errors.Is(err, domain.ErrNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		log.Printf("plant summary %s: %v", id, err)
		writeError(w, http.StatusInternalServerError, "failed to load plant summary")
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
