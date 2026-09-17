package normalize

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

// thermexRecord mirrors one element of the ThermexWatch "readings" array.
// timestampMs and level are decoded as `any`: ThermexWatch's own numeric
// severity scale can arrive as a non-numeric string in bad data, and that
// alone shouldn't sink an otherwise-usable reading.
type thermexRecord struct {
	ReadingID      string   `json:"readingId"`
	AssetCode      *string  `json:"assetCode"`
	ProductionLine string   `json:"productionLine"`
	TimestampMs    any      `json:"timestampMs"`
	AlertCode      string   `json:"alertCode"`
	Level          any      `json:"level"`
	VibrationG     *float64 `json:"vibration_g"`
	TemperatureF   *float64 `json:"temperature_f"`
	PowerKW        *float64 `json:"power_kw"`
	IsActive       bool     `json:"is_active"`
	SignalQuality  string   `json:"signal_quality"`
}

var knownAlertCodes = map[string]bool{
	"OK":         true,
	"VIB_WARN":   true,
	"TEMP_WARN":  true,
	"TEMP_CRIT":  true,
	"POWER_DROP": true,
}

// ThermexWatch normalizes one raw ThermexWatch reading. Vibration is kept
// in its native unit (g, acceleration) rather than converted to PulseForge's
// mm/s (velocity): the two aren't interconvertible without additional
// information (dominant frequency), so severity classification for
// ThermexWatch relies on the vendor's own level/alertCode instead of a
// cross-vendor numeric vibration threshold. Temperature is a straight unit
// conversion (F->C) and is normalized.
func ThermexWatch(raw json.RawMessage, ref *refdata.Store) Outcome {
	var rec thermexRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Outcome{Reject: fmt.Sprintf("malformed_json: %v", err)}
	}

	if rec.AssetCode == nil || *rec.AssetCode == "" {
		return Outcome{Reject: "unknown_machine_id: assetCode missing"}
	}
	asset, ok := ref.Machine(*rec.AssetCode)
	if !ok {
		return Outcome{Reject: fmt.Sprintf("unknown_machine_id: %q not in asset reference", *rec.AssetCode)}
	}

	ms, ok := asMillis(rec.TimestampMs)
	if !ok {
		return Outcome{Reject: fmt.Sprintf("invalid_event_time: timestampMs=%v", rec.TimestampMs)}
	}
	eventTime := time.UnixMilli(ms).UTC()

	var tempC *float64
	if rec.TemperatureF != nil {
		c := (*rec.TemperatureF - 32) * 5 / 9
		if !plausibleTempC(c) {
			return Outcome{Reject: fmt.Sprintf("temperature_out_of_range: %.1fF", *rec.TemperatureF)}
		}
		tempC = &c
	}

	var reasonParts []string
	attention, recognizedLevel := levelToAttention(rec.Level)
	if !recognizedLevel {
		reasonParts = append(reasonParts, fmt.Sprintf("unrecognized_level:%v", rec.Level))
		attention = alertCodeFallback(rec.AlertCode)
	}
	if !knownAlertCodes[rec.AlertCode] {
		reasonParts = append(reasonParts, fmt.Sprintf("unrecognized_alert_code:%s", rec.AlertCode))
	}
	if !rec.IsActive {
		// Vendor has marked this alert condition as no longer active; it
		// shouldn't contribute to current attention, only to history.
		attention = domain.AttentionNone
	}

	lineID := ref.ResolveLineID(asset.PlantID, rec.ProductionLine)
	if lineID == "" {
		lineID = asset.LineID
	}

	reasonCode := ""
	if attention >= domain.AttentionMedium {
		reasonCode = fmt.Sprintf("THERMEXWATCH_%s_L%v", rec.AlertCode, rec.Level)
	}
	if len(reasonParts) > 0 {
		if reasonCode != "" {
			reasonCode += "|"
		}
		reasonCode += strings.Join(reasonParts, ",")
	}

	machineState := "inactive"
	if rec.IsActive {
		machineState = "active"
	}

	event := domain.NormalizedEvent{
		ID:             newEventID(),
		Vendor:         domain.VendorThermexWatch,
		SourceEventID:  rec.ReadingID,
		MachineID:      asset.MachineID,
		PlantID:        asset.PlantID,
		LineID:         lineID,
		EventTime:      eventTime,
		EventType:      rec.AlertCode,
		AttentionLevel: attention,
		ReasonCode:     reasonCode,
		VibrationValue: rec.VibrationG,
		VibrationUnit:  "g",
		TemperatureC:   tempC,
		PowerKW:        rec.PowerKW,
		MachineState:   machineState,
		RawPayload:     append(json.RawMessage(nil), raw...),
	}
	return Outcome{Event: event}
}

func asMillis(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

func levelToAttention(v any) (domain.AttentionLevel, bool) {
	f, ok := v.(float64)
	if !ok {
		return domain.AttentionNone, false
	}
	switch int(f) {
	case 1:
		return domain.AttentionNone, true
	case 2:
		return domain.AttentionLow, true
	case 3:
		return domain.AttentionMedium, true
	case 4:
		return domain.AttentionHigh, true
	case 5:
		return domain.AttentionCritical, true
	default:
		return domain.AttentionNone, false
	}
}

func alertCodeFallback(code string) domain.AttentionLevel {
	switch code {
	case "OK":
		return domain.AttentionNone
	case "TEMP_CRIT":
		return domain.AttentionCritical
	case "VIB_WARN", "TEMP_WARN", "POWER_DROP":
		return domain.AttentionMedium
	default:
		return domain.AttentionNone
	}
}
