package normalize

import (
	"encoding/json"
	"testing"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

func testRefdata(t *testing.T) *refdata.Store {
	t.Helper()
	ref, err := refdata.Load()
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return ref
}

func TestPulseForge_HappyPath(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1001","machine_id":"EQ-001","line_id":"LINE-A",
		"event_time":"2026-04-18T07:59:12Z","event_type":"HIGH_VIBRATION","severity":"high",
		"vibration_mm_s":11.8,"temperature_c":83.2,"machine_state":"running",
		"sensor_health":0.91,"vendor_confidence":0.87
	}`)

	out := PulseForge(raw, ref)
	if out.Reject != "" {
		t.Fatalf("unexpected reject: %s", out.Reject)
	}
	ev := out.Event
	if ev.MachineID != "EQ-001" || ev.PlantID != "PLANT_01" || ev.LineID != "LINE-A" {
		t.Errorf("bad resolved identity: %+v", ev)
	}
	if ev.AttentionLevel != domain.AttentionHigh {
		t.Errorf("attention = %v, want high", ev.AttentionLevel)
	}
	if ev.VibrationUnit != "mm/s" {
		t.Errorf("vibration unit = %q, want mm/s", ev.VibrationUnit)
	}
	if ev.ReasonCode == "" {
		t.Errorf("expected a non-empty reason code for a high-severity event")
	}
}

func TestPulseForge_EmptyMachineID_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1201","machine_id":"","line_id":"LINE-A",
		"event_time":"2026-04-18T08:28:12Z","event_type":"HIGH_VIBRATION","severity":"high",
		"vibration_mm_s":10.9,"temperature_c":82.5
	}`)
	out := PulseForge(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for empty machine_id, got event: %+v", out.Event)
	}
}

func TestPulseForge_UnknownMachineID_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-9999","machine_id":"EQ-999","line_id":"LINE-A",
		"event_time":"2026-04-18T08:28:12Z","event_type":"HIGH_VIBRATION","severity":"high",
		"vibration_mm_s":10.9,"temperature_c":82.5
	}`)
	out := PulseForge(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for unknown machine_id")
	}
}

func TestPulseForge_NonISOTimestamp_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1202","machine_id":"EQ-002","line_id":"LINE-A",
		"event_time":"18-04-2026 08:28:20","event_type":"TEMP_SPIKE","severity":"urgent",
		"vibration_mm_s":6.0,"temperature_c":-999,"sensor_health":"bad","vendor_confidence":1.2
	}`)
	out := PulseForge(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for non-ISO event_time, got event: %+v", out.Event)
	}
}

func TestPulseForge_ImplausibleTemperature_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1203","machine_id":"EQ-002","line_id":"LINE-A",
		"event_time":"2026-04-18T08:28:20Z","event_type":"TEMP_SPIKE","severity":"high",
		"vibration_mm_s":6.0,"temperature_c":-999
	}`)
	out := PulseForge(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for -999C reading")
	}
}

func TestPulseForge_UnrecognizedSeverity_SoftNormalized(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1204","machine_id":"EQ-002","line_id":"LINE-A",
		"event_time":"2026-04-18T08:28:20Z","event_type":"TEMP_SPIKE","severity":"urgent",
		"vibration_mm_s":6.0,"temperature_c":70.0
	}`)
	out := PulseForge(raw, ref)
	if out.Reject != "" {
		t.Fatalf("unrecognized severity alone should not reject the record, got: %s", out.Reject)
	}
	if out.Event.AttentionLevel != domain.AttentionNone {
		t.Errorf("attention = %v, want none for unrecognized severity", out.Event.AttentionLevel)
	}
}

func TestPulseForge_BareLineLetter_ResolvedToCanonicalLineID(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"event_id":"PF-1301","machine_id":"EQ-004","line_id":"C",
		"event_time":"2026-04-18T08:28:20Z","event_type":"POWER_FLUCTUATION","severity":"medium",
		"vibration_mm_s":5.0,"temperature_c":70.0
	}`)
	out := PulseForge(raw, ref)
	if out.Reject != "" {
		t.Fatalf("unexpected reject: %s", out.Reject)
	}
	if out.Event.LineID != "LINE-C" {
		t.Errorf("line_id = %q, want LINE-C (resolved from bare letter C)", out.Event.LineID)
	}
}

func TestThermexWatch_HappyPath(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"readingId":"TW-8801","assetCode":"EQ-001","productionLine":"A",
		"timestampMs":1776499152000,"alertCode":"VIB_WARN","level":4,
		"vibration_g":0.81,"temperature_f":181.2,"power_kw":37.8,"is_active":true,"signal_quality":"GOOD"
	}`)
	out := ThermexWatch(raw, ref)
	if out.Reject != "" {
		t.Fatalf("unexpected reject: %s", out.Reject)
	}
	ev := out.Event
	if ev.LineID != "LINE-A" {
		t.Errorf("line_id = %q, want LINE-A (resolved from bare letter A)", ev.LineID)
	}
	if ev.AttentionLevel != domain.AttentionHigh {
		t.Errorf("attention = %v, want high for level 4", ev.AttentionLevel)
	}
	if ev.VibrationUnit != "g" {
		t.Errorf("vibration unit = %q, want g (not converted)", ev.VibrationUnit)
	}
	wantC := (181.2 - 32) * 5 / 9
	if ev.TemperatureC == nil || *ev.TemperatureC != wantC {
		t.Errorf("temperature_c = %v, want %v (converted from F)", ev.TemperatureC, wantC)
	}
}

func TestThermexWatch_NullAssetCode_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"readingId":"TW-9911","assetCode":null,"productionLine":"D",
		"timestampMs":"bad-ts","alertCode":"TEMP_WARN","level":"four",
		"vibration_g":0.4,"temperature_f":170.0,"is_active":true
	}`)
	out := ThermexWatch(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for null assetCode")
	}
}

func TestThermexWatch_UnrecognizedAlertCode_StillAccepted(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"readingId":"TW-9912","assetCode":"EQ-008","productionLine":"ZZ",
		"timestampMs":1776500220000,"alertCode":"UNKNOWN_ALERT","level":4,
		"vibration_g":0.29,"temperature_f":188.0,"power_kw":33.0,"is_active":true
	}`)
	out := ThermexWatch(raw, ref)
	if out.Reject != "" {
		t.Fatalf("an unrecognized alert code with a valid numeric level should not reject, got: %s", out.Reject)
	}
	if out.Event.AttentionLevel != domain.AttentionHigh {
		t.Errorf("attention = %v, want high (falls back to numeric level)", out.Event.AttentionLevel)
	}
	// productionLine "ZZ" doesn't resolve for PLANT_02; falls back to the
	// asset reference's own line id rather than being left empty/wrong.
	if out.Event.LineID != "LINE-E" {
		t.Errorf("line_id = %q, want LINE-E (asset reference fallback)", out.Event.LineID)
	}
}

func TestThermexWatch_InactiveAlert_AttentionSuppressed(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"readingId":"TW-8804","assetCode":"EQ-004","productionLine":"C",
		"timestampMs":1776498750000,"alertCode":"POWER_DROP","level":3,
		"vibration_g":0.21,"temperature_f":151.0,"power_kw":18.0,"is_active":false
	}`)
	out := ThermexWatch(raw, ref)
	if out.Reject != "" {
		t.Fatalf("unexpected reject: %s", out.Reject)
	}
	if out.Event.AttentionLevel != domain.AttentionNone {
		t.Errorf("attention = %v, want none for an inactive alert", out.Event.AttentionLevel)
	}
}

func TestThermexWatch_MalformedTimestamp_Rejected(t *testing.T) {
	ref := testRefdata(t)
	raw := json.RawMessage(`{
		"readingId":"TW-9921","assetCode":"EQ-005","productionLine":null,
		"timestampMs":"bad-time","alertCode":"TEMP_SPIKE","level":5,
		"is_active":true
	}`)
	out := ThermexWatch(raw, ref)
	if out.Reject == "" {
		t.Fatalf("expected reject for non-numeric timestampMs")
	}
}
