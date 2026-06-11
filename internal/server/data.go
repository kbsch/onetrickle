package server

import (
	"encoding/csv"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"

	"onetrickle/internal/consol"
	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/workflow"
)

// errCertified marks writes rejected because the target unit is certified.
var errCertified = errors.New("unit is certified; reopen it first")

// wfKey converts a unit key into its workflow key.
func wfKey(u cube.UnitKey) workflow.Key {
	return workflow.Key{Cube: u.Cube, Entity: u.Entity, Scenario: u.Scenario, Time: u.Time}
}

// validateWrite checks one cell write per SPEC §3: existing cube, leaf entity
// and scenario, an existing month, leaf coordinate members, a user-writable
// origin, no calculated account (dynamic-calc or stored-calc formula — the
// engine owns those values), a finite value, a valid IC partner, and a
// non-certified unit (errCertified). The coord must already be normalized.
func validateWrite(meta *model.Metadata, reg *workflow.Registry, wr cube.CellWrite) error {
	if _, err := meta.CubeOf(wr.Unit.Cube); err != nil {
		return err
	}
	if math.IsNaN(wr.Value) || math.IsInf(wr.Value, 0) {
		return fmt.Errorf("value %v is not a finite number", wr.Value)
	}
	leafChecks := []struct {
		dim model.DimType
		val string
	}{
		{model.DimEntity, wr.Unit.Entity},
		{model.DimScenario, wr.Unit.Scenario},
		{model.DimAccount, wr.Coord.Account},
		{model.DimFlow, wr.Coord.Flow},
		{model.DimUD1, wr.Coord.UD1},
		{model.DimUD2, wr.Coord.UD2},
		{model.DimUD3, wr.Coord.UD3},
		{model.DimUD4, wr.Coord.UD4},
	}
	for _, c := range leafChecks {
		d := meta.Dim(c.dim)
		if d == nil || !d.Has(c.val) {
			return fmt.Errorf("unknown %s %q", c.dim, c.val)
		}
		if !d.IsLeaf(c.val) {
			return fmt.Errorf("%s %q is not a leaf", c.dim, c.val)
		}
	}
	if !model.TimeIsMonth(wr.Unit.Time) {
		return fmt.Errorf("time %q is not a month member", wr.Unit.Time)
	}
	if d := meta.Dim(model.DimTime); d == nil || !d.Has(wr.Unit.Time) {
		return fmt.Errorf("unknown Time %q", wr.Unit.Time)
	}
	if a := meta.Account().Get(wr.Coord.Account); a != nil {
		if a.DynamicCalc {
			return fmt.Errorf("account %q is a dynamic-calc account and cannot be written", wr.Coord.Account)
		}
		if a.Formula != "" {
			return fmt.Errorf("account %q has a stored-calc formula and cannot be written", wr.Coord.Account)
		}
	}
	if !slices.Contains(model.UserOrigins, wr.Coord.Origin) {
		return fmt.Errorf("origin %q is not user-writable (want one of %v)", wr.Coord.Origin, model.UserOrigins)
	}
	if !meta.ValidIC(wr.Coord.IC) {
		return fmt.Errorf("invalid IC partner %q", wr.Coord.IC)
	}
	if reg.Status(wfKey(wr.Unit)) == workflow.StatusCertified {
		return fmt.Errorf("unit %s: %w", wr.Unit.Key(), errCertified)
	}
	return nil
}

func (s *Server) handleDataCells(w http.ResponseWriter, r *http.Request) {
	var writes []cube.CellWrite
	if err := decodeJSON(r, &writes); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// All-or-nothing: validate every write before applying any.
	for i := range writes {
		writes[i].Coord = writes[i].Coord.Normalize()
		if err := validateWrite(s.state.Meta, s.state.Workflow, writes[i]); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errCertified) {
				code = http.StatusConflict
			}
			writeError(w, code, fmt.Errorf("write %d: %w", i, err))
			return
		}
	}
	for _, wr := range writes {
		if wr.Value == 0 {
			if u := s.state.Cells.Unit(wr.Unit); u != nil {
				delete(u.Input, wr.Coord)
			}
			continue
		}
		s.state.Cells.Ensure(wr.Unit).Input[wr.Coord] = wr.Value
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "written": len(writes)})
}

func (s *Server) handleQuery(w http.ResponseWriter, r *http.Request) {
	var req cube.QueryRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Cube == "" {
		req.Cube = req.POV.Cube
	}
	if req.POV.Cube == "" {
		req.POV.Cube = req.Cube
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	res, err := s.engine.Query(s.state.Meta, req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cubeName, scenario, timeMonth := q.Get("cube"), q.Get("scenario"), q.Get("time")
	if cubeName == "" || scenario == "" || timeMonth == "" {
		writeError(w, http.StatusBadRequest, errors.New("cube, scenario and time query parameters are required"))
		return
	}
	stage := model.ConsStage(q.Get("stage"))
	if stage == "" {
		stage = model.StageConsolidated
	}
	switch stage {
	case model.StageLocal, model.StageTranslated, model.StageElimination, model.StageConsolidated:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown stage %q", q.Get("stage")))
		return
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := s.state.Cells.Keys(cube.UnitKey{Cube: cubeName, Scenario: scenario, Time: timeMonth})
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key() < keys[j].Key() })

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="onetrickle-export.csv"`)
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"entity", "account", "flow", "origin", "ic", "ud1", "ud2", "ud3", "ud4", "value"})
	for _, k := range keys {
		u := s.state.Cells.Unit(k)
		if u == nil {
			continue
		}
		var cells cube.CellMap
		if stage == model.StageLocal {
			cells = u.Input
		} else {
			cells = u.Stages[stage]
		}
		coords := make([]cube.CellCoord, 0, len(cells))
		for c := range cells {
			coords = append(coords, c)
		}
		sort.Slice(coords, func(i, j int) bool { return coords[i].Key() < coords[j].Key() })
		for _, c := range coords {
			v := cells[c]
			if v == 0 {
				continue
			}
			_ = cw.Write([]string{
				k.Entity, c.Account, c.Flow, c.Origin, c.IC,
				c.UD1, c.UD2, c.UD3, c.UD4,
				strconv.FormatFloat(v, 'g', -1, 64),
			})
		}
	}
	cw.Flush()
}

func (s *Server) handleProcess(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cube     string `json:"cube"`
		Scenario string `json:"scenario"`
		Time     string `json:"time"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := consol.Process(s.state.Meta, s.state.Cells, req.Cube, req.Scenario, req.Time)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
