// Package refdata loads the plant/line/machine master data that vendor
// payloads are resolved against. It is treated as the source of truth for
// plant_id/line_id: vendor-supplied plant/line fields are used only to
// sanity-check against it, never to override it.
package refdata

import (
	"embed"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
)

//go:embed data/asset_reference.csv data/line_reference.csv
var files embed.FS

// Asset is one row of asset_reference.csv.
type Asset struct {
	MachineID            string
	PlantID              string
	LineID               string
	MachineType          string
	Criticality          string
	RatedMaxTempC        float64
	RatedMaxVibrationMMs float64
	BaselinePowerKW      float64
	AssetStatus          string
}

// Line is one row of line_reference.csv.
type Line struct {
	PlantID         string
	LineID          string
	LineName        string
	OperatingWindow string
}

// Store is an immutable, in-memory lookup table built once at startup.
type Store struct {
	assets   map[string]Asset  // machine_id -> Asset
	lines    map[string]Line   // plant_id|line_id -> Line
	byLetter map[string]string // plant_id|letter -> line_id, e.g. "PLANT_01|A" -> "LINE-A"
}

// Load parses the embedded reference CSVs into a Store.
func Load() (*Store, error) {
	s := &Store{
		assets:   make(map[string]Asset),
		lines:    make(map[string]Line),
		byLetter: make(map[string]string),
	}

	if err := s.loadAssets(); err != nil {
		return nil, fmt.Errorf("refdata: load assets: %w", err)
	}
	if err := s.loadLines(); err != nil {
		return nil, fmt.Errorf("refdata: load lines: %w", err)
	}
	return s, nil
}

func (s *Store) loadAssets() error {
	rows, err := readCSV("data/asset_reference.csv")
	if err != nil {
		return err
	}
	for _, row := range rows {
		maxTemp, _ := strconv.ParseFloat(row["rated_max_temp_c"], 64)
		maxVib, _ := strconv.ParseFloat(row["rated_max_vibration_mm_s"], 64)
		basePower, _ := strconv.ParseFloat(row["baseline_power_kw"], 64)
		a := Asset{
			MachineID:            row["machine_id"],
			PlantID:              row["plant_id"],
			LineID:               row["line_id"],
			MachineType:          row["machine_type"],
			Criticality:          row["criticality"],
			RatedMaxTempC:        maxTemp,
			RatedMaxVibrationMMs: maxVib,
			BaselinePowerKW:      basePower,
			AssetStatus:          row["asset_status"],
		}
		s.assets[a.MachineID] = a
	}
	return nil
}

func (s *Store) loadLines() error {
	rows, err := readCSV("data/line_reference.csv")
	if err != nil {
		return err
	}
	for _, row := range rows {
		l := Line{
			PlantID:         row["plant_id"],
			LineID:          row["line_id"],
			LineName:        row["line_name"],
			OperatingWindow: row["operating_window"],
		}
		key := l.PlantID + "|" + l.LineID
		s.lines[key] = l

		// Vendors sometimes send a bare line letter ("A") instead of the
		// canonical line id ("LINE-A"). Index by trailing letter per plant
		// so ResolveLineID can map either form back to the canonical id.
		if idx := strings.LastIndex(l.LineID, "-"); idx != -1 && idx < len(l.LineID)-1 {
			letter := l.LineID[idx+1:]
			s.byLetter[l.PlantID+"|"+letter] = l.LineID
		}
	}
	return nil
}

// Machine looks up asset reference data by machine id.
func (s *Store) Machine(machineID string) (Asset, bool) {
	a, ok := s.assets[machineID]
	return a, ok
}

// Line looks up line reference data by plant id + canonical line id.
func (s *Store) Line(plantID, lineID string) (Line, bool) {
	l, ok := s.lines[plantID+"|"+lineID]
	return l, ok
}

// ResolveLineID normalizes a vendor-supplied line reference ("A", "LINE-A")
// to the canonical line id for a plant. Returns "" if it can't be resolved.
func (s *Store) ResolveLineID(plantID, raw string) string {
	if raw == "" {
		return ""
	}
	if _, ok := s.lines[plantID+"|"+raw]; ok {
		return raw
	}
	if canonical, ok := s.byLetter[plantID+"|"+raw]; ok {
		return canonical
	}
	return ""
}

// MachinesByPlant returns every known machine for a plant.
func (s *Store) MachinesByPlant(plantID string) []Asset {
	var out []Asset
	for _, a := range s.assets {
		if a.PlantID == plantID {
			out = append(out, a)
		}
	}
	return out
}

// AllMachines returns every known machine, for building a full list view.
func (s *Store) AllMachines() []Asset {
	out := make([]Asset, 0, len(s.assets))
	for _, a := range s.assets {
		out = append(out, a)
	}
	return out
}

func readCSV(path string) ([]map[string]string, error) {
	f, err := files.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return nil, err
	}

	var rows []map[string]string
	for {
		record, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		row := make(map[string]string, len(header))
		for i, col := range header {
			if i < len(record) {
				row[col] = record[i]
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}
