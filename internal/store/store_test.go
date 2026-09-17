package store

import (
	"testing"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	ref, err := refdata.Load()
	if err != nil {
		t.Fatalf("refdata.Load: %v", err)
	}
	return New(ref)
}

func TestSaveEvent_ExactIDDuplicate(t *testing.T) {
	s := testStore(t)
	ev := domain.NormalizedEvent{
		Vendor: domain.VendorPulseForge, SourceEventID: "PF-1001", MachineID: "EQ-001",
		EventType: "HIGH_VIBRATION", EventTime: time.Now(),
	}
	dup1, err := s.SaveEvent(ev)
	if err != nil || dup1 {
		t.Fatalf("first save: dup=%v err=%v, want dup=false err=nil", dup1, err)
	}
	dup2, err := s.SaveEvent(ev)
	if err != nil || !dup2 {
		t.Fatalf("resubmit with same event id: dup=%v err=%v, want dup=true err=nil", dup2, err)
	}
}

// Reproduces vendor_api_samples/duplicates.json: ThermexWatch resends the
// same reading under a new reading id (TW-8801-R). Different source id,
// identical content -- must still be caught as a duplicate.
func TestSaveEvent_ContentDuplicate_DifferentSourceID(t *testing.T) {
	s := testStore(t)
	eventTime := time.Now()
	original := domain.NormalizedEvent{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-8801", MachineID: "EQ-001",
		EventType: "VIB_WARN", EventTime: eventTime,
	}
	retry := domain.NormalizedEvent{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-8801-R", MachineID: "EQ-001",
		EventType: "VIB_WARN", EventTime: eventTime,
	}

	if dup, err := s.SaveEvent(original); dup || err != nil {
		t.Fatalf("original save: dup=%v err=%v", dup, err)
	}
	dup, err := s.SaveEvent(retry)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !dup {
		t.Errorf("retry with a new source id but identical content should be flagged duplicate")
	}
}

func TestGetMachineView_UnknownMachine(t *testing.T) {
	s := testStore(t)
	_, ok := s.GetMachineView("EQ-999")
	if ok {
		t.Errorf("expected ok=false for a machine not in the asset reference")
	}
}

func TestGetMachineView_KnownMachineNoData(t *testing.T) {
	s := testStore(t)
	view, ok := s.GetMachineView("EQ-003")
	if !ok {
		t.Fatalf("expected ok=true for a known machine even with no events yet")
	}
	if view.ProcessingStatus != "no_data" {
		t.Errorf("processing_status = %s, want no_data", view.ProcessingStatus)
	}
	if view.DerivedStatus != domain.DerivedUnknown {
		t.Errorf("derived_status = %s, want unknown", view.DerivedStatus)
	}
}

func TestPlantSummary_CountsAndCriticalMachines(t *testing.T) {
	s := testStore(t)
	ev := domain.NormalizedEvent{
		Vendor: domain.VendorThermexWatch, SourceEventID: "TW-1", MachineID: "EQ-002",
		EventType: "TEMP_CRIT", EventTime: time.Now(), AttentionLevel: domain.AttentionCritical,
		ReasonCode: "THERMEXWATCH_TEMP_CRIT_L5",
	}
	if _, err := s.SaveEvent(ev); err != nil {
		t.Fatalf("SaveEvent: %v", err)
	}
	s.RecomputeMachine("EQ-002")

	summary, err := s.PlantSummary("PLANT_01")
	if err != nil {
		t.Fatalf("PlantSummary: %v", err)
	}
	if summary.TotalMachines != 4 { // EQ-001..EQ-004 per asset_reference.csv
		t.Errorf("total_machines = %d, want 4", summary.TotalMachines)
	}
	if summary.CountByStatus[string(domain.DerivedCritical)] != 1 {
		t.Errorf("count_by_status[critical] = %d, want 1", summary.CountByStatus[string(domain.DerivedCritical)])
	}
	found := false
	for _, m := range summary.CriticalMachines {
		if m == "EQ-002" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected EQ-002 in critical_machines, got %v", summary.CriticalMachines)
	}
}

func TestPlantSummary_UnknownPlant(t *testing.T) {
	s := testStore(t)
	if _, err := s.PlantSummary("PLANT_99"); err == nil {
		t.Errorf("expected error for unknown plant_id")
	}
}
