// Package normalize maps each vendor's native payload shape onto the
// canonical domain.NormalizedEvent.
//
// Validation policy (kept deliberately simple and uniform across vendors):
//   - Hard reject (record is dropped, never retried) when identity or time
//     can't be established, or a core physical signal is outside a
//     plausible range: missing/unresolvable machine_id, unparseable event
//     time, or an implausible temperature. These make the record useless
//     for the derived view, so there's nothing to normalize.
//   - Soft normalize (record is kept, issue is flagged) for everything
//     else: unrecognized severity/alert enums, out-of-range confidence
//     scores, missing optional numeric fields. These degrade explainability
//     a little but don't invalidate the record.
package normalize

import (
	"crypto/rand"
	"encoding/hex"

	"aurik-equipment-monitor/internal/domain"
)

// Outcome is the result of normalizing one raw record. Exactly one of
// Event/Reject is meaningful: a non-empty Reject means the record was
// invalid and must not be stored or retried.
type Outcome struct {
	Event  domain.NormalizedEvent
	Reject string
}

func newEventID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "evt_" + hex.EncodeToString(b)
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// plausibleTempC is the range a manufacturing-floor machine's temperature
// reading can plausibly fall in. Anything outside it is treated as garbage
// (e.g. a -999 sentinel) rather than a real extreme reading.
func plausibleTempC(c float64) bool {
	return c >= -50 && c <= 300
}
