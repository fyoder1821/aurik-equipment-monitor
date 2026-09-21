// Package domain holds the canonical, vendor-agnostic types every layer of
// the service speaks: ingestion produces them, the worker enriches them, the
// API returns them.
package domain

import (
	"encoding/json"
	"errors"
	"time"
)

// ErrNotFound is a sentinel error store methods wrap to signal "this id
// doesn't exist" as distinct from an underlying storage failure, so
// httpapi can tell a 404 apart from a 500 without depending on the
// concrete store implementation.
var ErrNotFound = errors.New("not found")

type Vendor string

const (
	VendorPulseForge   Vendor = "pulseforge"
	VendorThermexWatch Vendor = "thermexwatch"
)

// AttentionLevel is the single normalized severity scale every vendor's
// native severity/level field is mapped onto.
type AttentionLevel int

const (
	AttentionNone AttentionLevel = iota
	AttentionLow
	AttentionMedium
	AttentionHigh
	AttentionCritical
)

var attentionNames = [...]string{"none", "low", "medium", "high", "critical"}

func (a AttentionLevel) String() string {
	if a < AttentionNone || int(a) >= len(attentionNames) {
		return "unknown"
	}
	return attentionNames[a]
}

// RecordStatus is the per-record ingestion/processing outcome, independent
// of the batch it arrived in.
type RecordStatus string

const (
	StatusQueued    RecordStatus = "queued"
	StatusProcessed RecordStatus = "processed"
	StatusDuplicate RecordStatus = "duplicate"
	StatusRejected  RecordStatus = "rejected"
	StatusFailed    RecordStatus = "failed"
)

// DerivedStatus is the coarse machine-level operational status shown to
// downstream consumers.
type DerivedStatus string

const (
	DerivedNormal           DerivedStatus = "normal"
	DerivedNeedsAttention   DerivedStatus = "needs_attention"
	DerivedCritical         DerivedStatus = "critical"
	DerivedUnderMaintenance DerivedStatus = "under_maintenance"
	DerivedUnknown          DerivedStatus = "unknown"
)

// NormalizedEvent is one vendor record (an event, reading, whatever the
// vendor calls it) mapped into the canonical schema. The vendor's own event
// code/alert code is preserved as EventType for traceability; AttentionLevel
// is the normalized dimension used for cross-vendor aggregation.
type NormalizedEvent struct {
	ID             string          `json:"id"`
	Vendor         Vendor          `json:"vendor"`
	SourceEventID  string          `json:"source_event_id"`
	MachineID      string          `json:"machine_id"`
	PlantID        string          `json:"plant_id"`
	LineID         string          `json:"line_id"`
	EventTime      time.Time       `json:"event_time"`
	IngestedAt     time.Time       `json:"ingested_at"`
	EventType      string          `json:"event_type"`
	AttentionLevel AttentionLevel  `json:"attention_level"`
	ReasonCode     string          `json:"reason_code,omitempty"`
	VibrationValue *float64        `json:"vibration_value,omitempty"`
	VibrationUnit  string          `json:"vibration_unit,omitempty"`
	TemperatureC   *float64        `json:"temperature_c,omitempty"`
	PowerKW        *float64        `json:"power_kw,omitempty"`
	SensorHealth   *float64        `json:"sensor_health,omitempty"`
	MachineState   string          `json:"machine_state,omitempty"`
	RawPayload     json.RawMessage `json:"raw_payload"`
}

// RecordOutcome is what the ingestion status endpoint reports for one
// record within a batch.
type RecordOutcome struct {
	SourceEventID string       `json:"source_event_id"`
	Vendor        Vendor       `json:"vendor"`
	Status        RecordStatus `json:"status"`
	Reason        string       `json:"reason,omitempty"`
	Attempts      int          `json:"attempts"`
	NormalizedID  string       `json:"normalized_id,omitempty"`
}

// Batch is one ingestion HTTP call and the fate of every record in it.
type Batch struct {
	ID          string          `json:"batch_id"`
	Vendor      Vendor          `json:"vendor"`
	ReceivedAt  time.Time       `json:"received_at"`
	RecordCount int             `json:"record_count"`
	Outcomes    []RecordOutcome `json:"outcomes"`
}

// EventRef points back at the raw record that contributed to a derived view.
type EventRef struct {
	Vendor        Vendor `json:"vendor"`
	SourceEventID string `json:"source_event_id"`
}

// SignalSnapshot is a compact view of the latest event from one vendor for
// a machine, shown inline on the machine view.
type SignalSnapshot struct {
	Vendor         Vendor    `json:"vendor"`
	EventType      string    `json:"event_type"`
	EventTime      time.Time `json:"event_time"`
	AttentionLevel string    `json:"attention_level"`
}

// MachineView is the derived machine operational attention view returned by
// the output API.
type MachineView struct {
	MachineID               string           `json:"machine_id"`
	PlantID                 string           `json:"plant_id"`
	LineID                  string           `json:"line_id"`
	DerivedStatus           DerivedStatus    `json:"derived_status"`
	NeedsAttention          bool             `json:"needs_attention"`
	AttentionLevel          string           `json:"attention_level"`
	ReasonCodes             []string         `json:"reason_codes"`
	LatestRelevantEventTime *time.Time       `json:"latest_relevant_event_time,omitempty"`
	ProcessingStatus        string           `json:"processing_status"`
	LastProcessedAt         *time.Time       `json:"last_processed_at,omitempty"`
	SourceEventRefs         []EventRef       `json:"source_event_refs"`
	LatestSignals           []SignalSnapshot `json:"latest_signals"`
}

// LineSummary rolls up machine attention counts for one production line.
type LineSummary struct {
	LineID                   string `json:"line_id"`
	LineName                 string `json:"line_name,omitempty"`
	MachinesNeedingAttention int    `json:"machines_needing_attention"`
}

// PlantSummary rolls up machine attention counts for one plant.
type PlantSummary struct {
	PlantID          string         `json:"plant_id"`
	TotalMachines    int            `json:"total_machines"`
	CountByStatus    map[string]int `json:"count_by_status"`
	CriticalMachines []string       `json:"critical_machines"`
	StaleMachines    []string       `json:"stale_machines,omitempty"`
	Lines            []LineSummary  `json:"lines,omitempty"`
}
