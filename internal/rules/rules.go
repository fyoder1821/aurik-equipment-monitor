// Package rules computes the derived machine operational attention view.
// It is deterministic by design (per the assessment brief: no ML, explainable
// logic) -- the same set of normalized events for a machine always produces
// the same view.
package rules

import (
	"sort"
	"time"

	"aurik-equipment-monitor/internal/domain"
	"aurik-equipment-monitor/internal/refdata"
)

// freshnessWindow: if the latest contributing signal is older than this,
// the view is reported as "stale" rather than "fresh" -- the status is
// still the best available answer, just flagged as possibly outdated.
const freshnessWindow = 2 * time.Hour

// Derive builds the operational view for one machine from every normalized
// event seen for it so far, plus its static reference data (rated
// thresholds, current asset/maintenance status).
func Derive(machineID string, events []domain.NormalizedEvent, asset refdata.Asset, now time.Time) domain.MachineView {
	view := domain.MachineView{
		MachineID:       machineID,
		PlantID:         asset.PlantID,
		LineID:          asset.LineID,
		ReasonCodes:     []string{},
		SourceEventRefs: []domain.EventRef{},
		LatestSignals:   []domain.SignalSnapshot{},
	}

	if len(events) == 0 {
		view.DerivedStatus = domain.DerivedUnknown
		view.AttentionLevel = domain.AttentionNone.String()
		view.ProcessingStatus = "no_data"
		return view
	}

	// Latest event per vendor, keyed by event_time rather than arrival
	// order, so a delayed/out-of-order delivery can't make a stale reading
	// look like the current one.
	latestByVendor := map[domain.Vendor]domain.NormalizedEvent{}
	for _, e := range events {
		cur, ok := latestByVendor[e.Vendor] // e.Vendor = pulseforge/thermexwatch
		if !ok || e.EventTime.After(cur.EventTime) {
			latestByVendor[e.Vendor] = e
		}
	}

	overall := domain.AttentionNone
	var latestTime time.Time
	reasonSet := map[string]bool{}

	for _, e := range latestByVendor {
		if e.AttentionLevel > overall {
			overall = e.AttentionLevel
		}
		if e.ReasonCode != "" {
			reasonSet[e.ReasonCode] = true
		}
		if e.EventTime.After(latestTime) {
			latestTime = e.EventTime
		}
		view.SourceEventRefs = append(view.SourceEventRefs, domain.EventRef{
			Vendor:        e.Vendor,
			SourceEventID: e.SourceEventID,
		})
		view.LatestSignals = append(view.LatestSignals, domain.SignalSnapshot{
			Vendor:         e.Vendor,
			EventType:      e.EventType,
			EventTime:      e.EventTime,
			AttentionLevel: e.AttentionLevel.String(),
		})

		// Cross-check against rated thresholds from asset reference data.
		// Only compared where the unit actually matches: temperature_c is
		// normalized for every vendor, but vibration is only compared for
		// PulseForge (mm/s, same unit as rated_max_vibration_mm_s).
		// ThermexWatch reports vibration in g (acceleration), a different
		// physical quantity that can't be compared to a mm/s (velocity)
		// threshold without extra information, so it's deliberately
		// excluded from this check.
		if e.TemperatureC != nil && asset.RatedMaxTempC > 0 && *e.TemperatureC > asset.RatedMaxTempC {
			reasonSet["TEMP_ABOVE_RATED_MAX"] = true
			overall = maxAttention(overall, domain.AttentionHigh)
		}
		if e.Vendor == domain.VendorPulseForge && e.VibrationValue != nil &&
			asset.RatedMaxVibrationMMs > 0 && *e.VibrationValue > asset.RatedMaxVibrationMMs {
			reasonSet["VIBRATION_ABOVE_RATED_MAX"] = true
			overall = maxAttention(overall, domain.AttentionHigh)
		}
	}

	for code := range reasonSet {
		view.ReasonCodes = append(view.ReasonCodes, code)
	}
	sort.Strings(view.ReasonCodes)
	sort.Slice(view.SourceEventRefs, func(i, j int) bool {
		return view.SourceEventRefs[i].Vendor < view.SourceEventRefs[j].Vendor
	})
	sort.Slice(view.LatestSignals, func(i, j int) bool {
		return view.LatestSignals[i].Vendor < view.LatestSignals[j].Vendor
	})

	view.AttentionLevel = overall.String()
	// NeedsAttention/AttentionLevel reflect the raw signal picture;
	// DerivedStatus below layers the maintenance-workflow context on top,
	// so a consumer can see both "what the sensors say" and "what that
	// means operationally" without losing either.
	view.NeedsAttention = overall >= domain.AttentionMedium

	lt := latestTime
	view.LatestRelevantEventTime = &lt
	processedAt := now
	view.LastProcessedAt = &processedAt

	switch {
	case asset.AssetStatus == "maintenance":
		view.DerivedStatus = domain.DerivedUnderMaintenance
	case overall == domain.AttentionCritical:
		view.DerivedStatus = domain.DerivedCritical
	case overall >= domain.AttentionMedium:
		view.DerivedStatus = domain.DerivedNeedsAttention
	default:
		view.DerivedStatus = domain.DerivedNormal
	}

	if now.Sub(latestTime) > freshnessWindow {
		view.ProcessingStatus = "stale"
	} else {
		view.ProcessingStatus = "fresh"
	}

	return view
}

func maxAttention(a, b domain.AttentionLevel) domain.AttentionLevel {
	if b > a {
		return b
	}
	return a
}
