package rules

import (
	"testing"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return tm
}

func TestDerive_NoEvents_Unknown(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-001", PlantID: "PLANT_01", LineID: "LINE-A"}
	view := Derive("EQ-001", nil, asset, time.Now())
	if view.DerivedStatus != domain.DerivedUnknown {
		t.Errorf("derived_status = %s, want unknown", view.DerivedStatus)
	}
	if view.ProcessingStatus != "no_data" {
		t.Errorf("processing_status = %s, want no_data", view.ProcessingStatus)
	}
}

// Reproduces vendor_api_samples/out_of_order.json: a later event_time
// record arrives first, an earlier event_time record for the same vendor
// arrives second. The derived view must reflect whichever event has the
// latest event_time, not whichever was processed last.
func TestDerive_OutOfOrderArrival_UsesLatestEventTime(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-001", PlantID: "PLANT_01", LineID: "LINE-A", RatedMaxVibrationMMs: 9.0}
	now := mustTime(t, "2026-04-18T09:00:00Z")

	earlierHighVibration := domain.NormalizedEvent{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1301", MachineID: "EQ-001",
		EventTime: mustTime(t, "2026-04-18T08:31:02Z"), EventType: "HIGH_VIBRATION",
		AttentionLevel: domain.AttentionHigh, ReasonCode: "PULSEFORGE_HIGH_VIBRATION_HIGH",
	}
	laterRecovery := domain.NormalizedEvent{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1302", MachineID: "EQ-001",
		EventTime: mustTime(t, "2026-04-18T08:32:05Z"), EventType: "RECOVERY_SIGNAL",
		AttentionLevel: domain.AttentionLow,
	}

	// Processed (appended) in arrival order: PF-1302 first, PF-1301 second --
	// the reverse of event_time order.
	events := []domain.NormalizedEvent{laterRecovery, earlierHighVibration}

	view := Derive("EQ-001", events, asset, now)
	if view.AttentionLevel != domain.AttentionLow.String() {
		t.Errorf("attention_level = %s, want low (from the event with the later event_time)", view.AttentionLevel)
	}
	if view.DerivedStatus != domain.DerivedNormal {
		t.Errorf("derived_status = %s, want normal", view.DerivedStatus)
	}
	if !view.LatestRelevantEventTime.Equal(laterRecovery.EventTime) {
		t.Errorf("latest_relevant_event_time = %s, want %s", view.LatestRelevantEventTime, laterRecovery.EventTime)
	}
}

// Reproduces vendor_api_samples/conflicting_updates.json: two vendors
// report on the same machine around the same time with different
// severities. Worst-wins aggregation should surface the higher one, and
// both should appear in source_event_refs for traceability.
func TestDerive_ConflictingVendors_WorstWins(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-008", PlantID: "PLANT_02", LineID: "LINE-E"}
	now := mustTime(t, "2026-04-18T09:00:00Z")

	pulseforgeLow := domain.NormalizedEvent{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1401", MachineID: "EQ-008",
		EventTime: mustTime(t, "2026-04-18T08:39:10Z"), EventType: "RECOVERY_SIGNAL",
		AttentionLevel: domain.AttentionLow,
	}
	thermexCritical := domain.NormalizedEvent{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-9921", MachineID: "EQ-008",
		EventTime: mustTime(t, "2026-04-18T08:42:30Z"), EventType: "TEMP_CRIT",
		AttentionLevel: domain.AttentionCritical, ReasonCode: "THERMEXWATCH_TEMP_CRIT_L5",
	}

	view := Derive("EQ-008", []domain.NormalizedEvent{pulseforgeLow, thermexCritical}, asset, now)
	if view.DerivedStatus != domain.DerivedCritical {
		t.Errorf("derived_status = %s, want critical (worst of the two vendors)", view.DerivedStatus)
	}
	if len(view.SourceEventRefs) != 2 {
		t.Errorf("source_event_refs has %d entries, want 2 (one per vendor)", len(view.SourceEventRefs))
	}
}

func TestDerive_AssetUnderMaintenance_OverridesStatus(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-007", PlantID: "PLANT_02", LineID: "LINE-E", AssetStatus: "maintenance"}
	now := mustTime(t, "2026-04-18T09:00:00Z")
	events := []domain.NormalizedEvent{{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-8807", MachineID: "EQ-007",
		EventTime: mustTime(t, "2026-04-18T08:00:00Z"), EventType: "OK",
		AttentionLevel: domain.AttentionNone,
	}}
	view := Derive("EQ-007", events, asset, now)
	if view.DerivedStatus != domain.DerivedUnderMaintenance {
		t.Errorf("derived_status = %s, want under_maintenance", view.DerivedStatus)
	}
}

func TestDerive_StaleWhenOlderThanFreshnessWindow(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-001", PlantID: "PLANT_01", LineID: "LINE-A"}
	eventTime := mustTime(t, "2026-04-18T08:00:00Z")
	now := eventTime.Add(3 * time.Hour)
	events := []domain.NormalizedEvent{{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1", MachineID: "EQ-001",
		EventTime: eventTime, EventType: "RECOVERY_SIGNAL", AttentionLevel: domain.AttentionLow,
	}}
	view := Derive("EQ-001", events, asset, now)
	if view.ProcessingStatus != "stale" {
		t.Errorf("processing_status = %s, want stale", view.ProcessingStatus)
	}
}

func TestDerive_RatedVibrationBreach_PulseForgeOnly(t *testing.T) {
	asset := refdata.Asset{MachineID: "EQ-001", PlantID: "PLANT_01", LineID: "LINE-A", RatedMaxVibrationMMs: 9.0}
	now := mustTime(t, "2026-04-18T08:00:00Z")

	over := 12.0
	pfEvent := domain.NormalizedEvent{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1", MachineID: "EQ-001",
		EventTime: now, EventType: "HIGH_VIBRATION", AttentionLevel: domain.AttentionLow,
		VibrationValue: &over, VibrationUnit: "mm/s",
	}
	view := Derive("EQ-001", []domain.NormalizedEvent{pfEvent}, asset, now)
	found := false
	for _, code := range view.ReasonCodes {
		if code == "VIBRATION_ABOVE_RATED_MAX" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected VIBRATION_ABOVE_RATED_MAX reason code, got %v", view.ReasonCodes)
	}

	// A ThermexWatch reading with a numerically larger "g" value must NOT
	// trigger the same mm/s threshold -- the units aren't comparable.
	twEvent := domain.NormalizedEvent{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-1", MachineID: "EQ-001",
		EventTime: now, EventType: "VIB_WARN", AttentionLevel: domain.AttentionLow,
		VibrationValue: &over, VibrationUnit: "g",
	}
	view2 := Derive("EQ-001", []domain.NormalizedEvent{twEvent}, asset, now)
	for _, code := range view2.ReasonCodes {
		if code == "VIBRATION_ABOVE_RATED_MAX" {
			t.Errorf("ThermexWatch vibration (g) must not be compared against the mm/s threshold")
		}
	}
}
