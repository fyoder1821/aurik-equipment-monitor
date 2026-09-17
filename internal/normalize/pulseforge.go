package normalize

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

// pulseForgeRecord mirrors one element of the PulseForge "events" array.
// Fragile/optional fields (sensor_health) are decoded as `any` so a
// type-mismatched value (e.g. the string "bad") never fails the whole
// record; it's just treated as absent.
type pulseForgeRecord struct {
	EventID      string   `json:"event_id"`
	MachineID    string   `json:"machine_id"`
	LineID       string   `json:"line_id"`
	EventTime    string   `json:"event_time"`
	EventType    string   `json:"event_type"`
	Severity     string   `json:"severity"`
	VibrationMMs *float64 `json:"vibration_mm_s"`
	TemperatureC *float64 `json:"temperature_c"`
	MachineState string   `json:"machine_state"`
	SensorHealth any      `json:"sensor_health"`
}

// PulseForge normalizes one raw PulseForge event record. See the package
// doc comment for the hard-reject vs soft-normalize policy.
func PulseForge(raw json.RawMessage, ref *refdata.Store) Outcome {
	var rec pulseForgeRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Outcome{Reject: fmt.Sprintf("malformed_json: %v", err)}
	}

	if rec.MachineID == "" {
		return Outcome{Reject: "unknown_machine_id: machine_id missing"}
	}
	asset, ok := ref.Machine(rec.MachineID)
	if !ok {
		return Outcome{Reject: fmt.Sprintf("unknown_machine_id: %q not in asset reference", rec.MachineID)}
	}

	eventTime, err := time.Parse(time.RFC3339, rec.EventTime)
	if err != nil {
		return Outcome{Reject: fmt.Sprintf("invalid_event_time: %q", rec.EventTime)}
	}

	if rec.TemperatureC != nil && !plausibleTempC(*rec.TemperatureC) {
		return Outcome{Reject: fmt.Sprintf("temperature_out_of_range: %.1f", *rec.TemperatureC)}
	}

	attention, recognized := severityToAttention(rec.Severity)

	lineID := ref.ResolveLineID(asset.PlantID, rec.LineID)
	if lineID == "" {
		lineID = asset.LineID
	}

	reasonCode := ""
	switch {
	case !recognized:
		reasonCode = fmt.Sprintf("PULSEFORGE_UNRECOGNIZED_SEVERITY:%s", rec.Severity)
	case attention >= domain.AttentionMedium:
		reasonCode = fmt.Sprintf("PULSEFORGE_%s_%s", rec.EventType, strings.ToUpper(rec.Severity))
	}

	var sensorHealth *float64
	if f, ok := rec.SensorHealth.(float64); ok {
		v := clamp01(f)
		sensorHealth = &v
	}

	event := domain.NormalizedEvent{
		ID:             newEventID(),
		Vendor:         domain.VendorPulseForge,
		SourceEventID:  rec.EventID,
		MachineID:      asset.MachineID,
		PlantID:        asset.PlantID,
		LineID:         lineID,
		EventTime:      eventTime.UTC(),
		EventType:      rec.EventType,
		AttentionLevel: attention,
		ReasonCode:     reasonCode,
		VibrationValue: rec.VibrationMMs,
		VibrationUnit:  "mm/s",
		TemperatureC:   rec.TemperatureC,
		SensorHealth:   sensorHealth,
		MachineState:   rec.MachineState,
		RawPayload:     append(json.RawMessage(nil), raw...),
	}
	return Outcome{Event: event}
}

func severityToAttention(s string) (domain.AttentionLevel, bool) {
	switch strings.ToLower(s) {
	case "":
		return domain.AttentionNone, true
	case "low":
		return domain.AttentionLow, true
	case "medium":
		return domain.AttentionMedium, true
	case "high":
		return domain.AttentionHigh, true
	case "critical":
		return domain.AttentionCritical, true
	default:
		return domain.AttentionNone, false
	}
}
