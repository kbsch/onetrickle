package server

import (
	"fmt"
	"net/http"
	"slices"
	"sort"

	"onetrickle/internal/calc"
	"onetrickle/internal/cube"
	"onetrickle/internal/model"
)

// noneProtectedDims are the dimensions whose None root is mandatory (SPEC §2):
// every stored cell normalizes empty Flow/UD coordinates to None, so deleting
// or re-parenting that member would orphan all data and block every write.
var noneProtectedDims = []model.DimType{
	model.DimFlow, model.DimUD1, model.DimUD2, model.DimUD3, model.DimUD4,
}

// isProtectedNone reports whether (dt, name) is a mandatory None root.
func isProtectedNone(dt model.DimType, name string) bool {
	return name == model.NoneMember && slices.Contains(noneProtectedDims, dt)
}

// storedFormulaAccount reports whether the member has a stored-calc (i.e.
// non-dynamic) formula. Stored calcs must target leaf accounts: a Calc cell
// written at a non-leaf would double-count with its children in every rollup.
func storedFormulaAccount(m *model.Member) bool {
	return m != nil && m.Formula != "" && !m.DynamicCalc
}

// coordField returns the coordinate value of c addressed by a coordinate
// dimension ("" for unit-key dimensions, which are checked separately).
func coordField(c cube.CellCoord, dt model.DimType) string {
	switch dt {
	case model.DimAccount:
		return c.Account
	case model.DimFlow:
		return c.Flow
	case model.DimUD1:
		return c.UD1
	case model.DimUD2:
		return c.UD2
	case model.DimUD3:
		return c.UD3
	case model.DimUD4:
		return c.UD4
	}
	return ""
}

// inputRefCount counts stored Input cells that reference any of the names
// within dimension dt: as a coordinate value, as the unit-key entity or
// scenario, or as an IC partner (entities). Caller must hold the write lock.
func (s *Server) inputRefCount(dt model.DimType, names map[string]bool) int {
	n := 0
	for _, k := range s.state.Cells.Keys(cube.UnitKey{}) {
		u := s.state.Cells.Unit(k)
		if u == nil {
			continue
		}
		switch dt {
		case model.DimEntity:
			if names[k.Entity] {
				n += len(u.Input)
				continue
			}
			for c := range u.Input {
				if names[c.IC] {
					n++
				}
			}
		case model.DimScenario:
			if names[k.Scenario] {
				n += len(u.Input)
			}
		default:
			for c := range u.Input {
				if names[coordField(c, dt)] {
					n++
				}
			}
		}
	}
	return n
}

// purgeDerivedRefs removes engine-derived leftovers referencing deleted
// members: whole units keyed on a deleted entity/scenario (their Input is
// empty — inputRefCount gated the delete), stage cells whose coordinate or IC
// partner names a deleted member, and workflow entries of deleted units.
// Caller must hold the write lock.
func (s *Server) purgeDerivedRefs(dt model.DimType, names map[string]bool) {
	for _, k := range s.state.Cells.Keys(cube.UnitKey{}) {
		u := s.state.Cells.Unit(k)
		if u == nil {
			continue
		}
		if (dt == model.DimEntity && names[k.Entity]) || (dt == model.DimScenario && names[k.Scenario]) {
			s.state.Cells.Delete(k)
			continue
		}
		for _, cm := range u.Stages {
			for c := range cm {
				if names[coordField(c, dt)] || (dt == model.DimEntity && names[c.IC]) {
					delete(cm, c)
				}
			}
		}
	}
	if dt == model.DimEntity || dt == model.DimScenario {
		for ks, e := range s.state.Workflow.Entries {
			if e == nil {
				continue
			}
			if (dt == model.DimEntity && names[e.Key.Entity]) || (dt == model.DimScenario && names[e.Key.Scenario]) {
				delete(s.state.Workflow.Entries, ks)
			}
		}
	}
}

// memberDTO is the wire shape of one dimension member in the flat pre-order
// member tree (GET /api/dims/{type}/members) and of member mutations' replies.
type memberDTO struct {
	Name         string            `json:"name"`
	Parent       string            `json:"parent,omitempty"`
	Depth        int               `json:"depth"`
	Weight       float64           `json:"weight"`
	IsLeaf       bool              `json:"isLeaf"`
	Description  string            `json:"description,omitempty"`
	AccountType  model.AccountType `json:"accountType,omitempty"`
	IsIC         bool              `json:"isIC,omitempty"`
	DynamicCalc  bool              `json:"dynamicCalc,omitempty"`
	Formula      string            `json:"formula,omitempty"`
	Currency     string            `json:"currency,omitempty"`
	OwnershipPct float64           `json:"ownershipPct,omitempty"`
}

func memberToDTO(m *model.Member, depth int) memberDTO {
	return memberDTO{
		Name:         m.Name,
		Parent:       m.Parent,
		Depth:        depth,
		Weight:       m.Weight,
		IsLeaf:       len(m.Children) == 0,
		Description:  m.Description,
		AccountType:  m.AccountType,
		IsIC:         m.IsIC,
		DynamicCalc:  m.DynamicCalc,
		Formula:      m.Formula,
		Currency:     m.Currency,
		OwnershipPct: m.OwnershipPct,
	}
}

// dimOf resolves the {type} path segment to a stored dimension. ok is false
// for unknown dimension types (View and IC are implicit, never stored).
func (s *Server) dimOf(t string) (*model.Dimension, model.DimType, bool) {
	dt := model.DimType(t)
	if !slices.Contains(model.AllDims, dt) {
		return nil, "", false
	}
	return s.state.Meta.Dim(dt), dt, true
}

func (s *Server) handleMeta(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	meta := s.state.Meta

	cubes := make([]string, 0, len(meta.Cubes))
	for name := range meta.Cubes {
		cubes = append(cubes, name)
	}
	sort.Strings(cubes)

	scenarios := []string{}
	if d := meta.Dim(model.DimScenario); d != nil {
		for name := range d.Members {
			if d.IsLeaf(name) {
				scenarios = append(scenarios, name)
			}
		}
	}
	sort.Strings(scenarios)

	years := model.TimeYears(meta.Dim(model.DimTime))
	if years == nil {
		years = []int{}
	}
	currencies := meta.Currencies()
	if currencies == nil {
		currencies = []string{}
	}

	// Latest month with any stored data, so the UI can default its POV to a
	// period that actually shows numbers ("" when the store is empty).
	latest, latestIdx := "", -1
	for _, k := range s.state.Cells.Keys(cube.UnitKey{}) {
		u := s.state.Cells.Unit(k)
		if u == nil || (len(u.Input) == 0 && len(u.Stages) == 0) {
			continue
		}
		if idx := model.MonthIndex(k.Time); idx > latestIdx {
			latestIdx, latest = idx, k.Time
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cubes":          cubes,
		"scenarios":      scenarios,
		"years":          years,
		"currencies":     currencies,
		"latestDataTime": latest,
	})
}

func (s *Server) handleDimMembers(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d, _, ok := s.dimOf(r.PathValue("type"))
	if !ok || d == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown dimension %q", r.PathValue("type")))
		return
	}
	out := []memberDTO{}
	var walk func(name string, depth int)
	walk = func(name string, depth int) {
		m := d.Get(name)
		if m == nil {
			return
		}
		out = append(out, memberToDTO(m, depth))
		for _, c := range m.Children {
			walk(c, depth+1)
		}
	}
	for _, root := range d.Roots {
		walk(root, 0)
	}
	writeJSON(w, http.StatusOK, out)
}

// editableDim resolves {type} for a mutating member endpoint: 404 for unknown
// dimensions, 403 for dimensions users may not manage (Time, Origin).
func (s *Server) editableDim(w http.ResponseWriter, t string) (*model.Dimension, model.DimType, bool) {
	d, dt, ok := s.dimOf(t)
	if !ok || d == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown dimension %q", t))
		return nil, "", false
	}
	if !slices.Contains(model.UserEditableDims, dt) {
		writeError(w, http.StatusForbidden, fmt.Errorf("dimension %s is not user-editable", dt))
		return nil, "", false
	}
	return d, dt, true
}

// memberCreate is the POST /api/dims/{type}/members request body.
type memberCreate struct {
	Name         string  `json:"name"`
	Parent       string  `json:"parent"`
	Description  string  `json:"description"`
	Weight       float64 `json:"weight"`
	AccountType  string  `json:"accountType"`
	IsIC         bool    `json:"isIC"`
	DynamicCalc  bool    `json:"dynamicCalc"`
	Formula      string  `json:"formula"`
	Currency     string  `json:"currency"`
	OwnershipPct float64 `json:"ownershipPct"`
}

func (s *Server) handleDimAdd(w http.ResponseWriter, r *http.Request) {
	var req memberCreate
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, dt, ok := s.editableDim(w, r.PathValue("type"))
	if !ok {
		return
	}
	if dt == model.DimScenario && req.Parent != "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("the Scenario dimension is flat; member %q cannot have a parent", req.Name))
		return
	}
	if dt == model.DimAccount && req.AccountType != "" && !model.AccountType(req.AccountType).Valid() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid account type %q", req.AccountType))
		return
	}
	if req.Formula != "" {
		if _, err := calc.Parse(req.Formula); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	if dt == model.DimAccount && req.Parent != "" && storedFormulaAccount(d.Get(req.Parent)) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("cannot add a child under account %q: it has a stored-calc formula and must stay a leaf", req.Parent))
		return
	}
	m := &model.Member{
		Name:         req.Name,
		Parent:       req.Parent,
		Description:  req.Description,
		Weight:       req.Weight,
		AccountType:  model.AccountType(req.AccountType),
		IsIC:         req.IsIC,
		DynamicCalc:  req.DynamicCalc,
		Formula:      req.Formula,
		Currency:     req.Currency,
		OwnershipPct: req.OwnershipPct,
	}
	if err := d.AddMember(m); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("add member to %s: %w", dt, err))
		return
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, memberToDTO(m, len(d.PathToRoot(m.Name))-1))
}

// memberUpdate is the PUT /api/dims/{type}/members/{name} request body.
// Pointer fields distinguish "absent: leave unchanged" from explicit values.
type memberUpdate struct {
	Parent       *string  `json:"parent"`
	Description  *string  `json:"description"`
	Weight       *float64 `json:"weight"`
	AccountType  *string  `json:"accountType"`
	IsIC         *bool    `json:"isIC"`
	DynamicCalc  *bool    `json:"dynamicCalc"`
	Formula      *string  `json:"formula"`
	Currency     *string  `json:"currency"`
	OwnershipPct *float64 `json:"ownershipPct"`
}

func (s *Server) handleDimUpdate(w http.ResponseWriter, r *http.Request) {
	var req memberUpdate
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, dt, ok := s.editableDim(w, r.PathValue("type"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	m := d.Get(name)
	if m == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("member %q not found in %s", name, dt))
		return
	}
	if req.AccountType != nil && *req.AccountType != "" && dt == model.DimAccount && !model.AccountType(*req.AccountType).Valid() {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid account type %q", *req.AccountType))
		return
	}
	if req.Formula != nil && *req.Formula != "" {
		if _, err := calc.Parse(*req.Formula); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
	}
	// Stored-calc formulas must target leaf accounts (prospective values:
	// fields absent from the request keep the member's current value).
	if dt == model.DimAccount {
		formula, dynamic := m.Formula, m.DynamicCalc
		if req.Formula != nil {
			formula = *req.Formula
		}
		if req.DynamicCalc != nil {
			dynamic = *req.DynamicCalc
		}
		if formula != "" && !dynamic && !d.IsLeaf(name) {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("account %q is not a leaf; stored-calc formulas must target leaf accounts (use a dynamic formula instead)", name))
			return
		}
	}
	if req.Parent != nil && *req.Parent != m.Parent {
		if dt == model.DimScenario && *req.Parent != "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("the Scenario dimension is flat; member %q cannot have a parent", name))
			return
		}
		if isProtectedNone(dt, name) && *req.Parent != "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("member %q of %s is the mandatory default root and cannot be moved", name, dt))
			return
		}
		if dt == model.DimAccount && *req.Parent != "" && storedFormulaAccount(d.Get(*req.Parent)) {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("cannot move %q under account %q: it has a stored-calc formula and must stay a leaf", name, *req.Parent))
			return
		}
		if err := d.MoveMember(name, *req.Parent); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("move member: %w", err))
			return
		}
	}
	if req.Description != nil {
		m.Description = *req.Description
	}
	if req.Weight != nil {
		m.Weight = *req.Weight
	}
	if req.AccountType != nil {
		m.AccountType = model.AccountType(*req.AccountType)
	}
	if req.IsIC != nil {
		m.IsIC = *req.IsIC
	}
	if req.DynamicCalc != nil {
		m.DynamicCalc = *req.DynamicCalc
	}
	if req.Formula != nil {
		m.Formula = *req.Formula
	}
	if req.Currency != nil {
		m.Currency = *req.Currency
	}
	if req.OwnershipPct != nil {
		m.OwnershipPct = *req.OwnershipPct
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, memberToDTO(m, len(d.PathToRoot(name))-1))
}

func (s *Server) handleDimDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, dt, ok := s.editableDim(w, r.PathValue("type"))
	if !ok {
		return
	}
	name := r.PathValue("name")
	if !d.Has(name) {
		writeError(w, http.StatusNotFound, fmt.Errorf("member %q not found in %s", name, dt))
		return
	}
	if isProtectedNone(dt, name) {
		writeError(w, http.StatusBadRequest,
			fmt.Errorf("member %q of %s is the mandatory default root and cannot be deleted", name, dt))
		return
	}

	rec := r.URL.Query().Get("recursive")
	recursive := rec == "1" || rec == "true"
	if !recursive && len(d.Get(name).Children) > 0 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("remove member: member %q has children; use recursive delete", name))
		return
	}

	// Referential integrity: refuse while user-entered cells still reference
	// the member (or any member of the deleted subtree) as a coordinate, unit
	// key or IC partner — orphaned cells would survive in exports/queries and
	// silently resurrect if the member were re-created.
	doomed := map[string]bool{name: true}
	for _, c := range d.Descendants(name) {
		doomed[c] = true
	}
	if n := s.inputRefCount(dt, doomed); n > 0 {
		writeError(w, http.StatusConflict,
			fmt.Errorf("cannot delete %s %q: %d stored cell(s) still reference it; clear or reload the data first", dt, name, n))
		return
	}

	if err := d.RemoveMember(name, recursive); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("remove member: %w", err))
		return
	}
	s.purgeDerivedRefs(dt, doomed)
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// rateDTO is one FX rate on the wire (GET and PUT /api/rates).
type rateDTO struct {
	Currency string         `json:"currency"`
	Type     model.RateType `json:"type"`
	Value    float64        `json:"value"`
}

// ratesFor lists the rates of one (scenario, time), sorted by currency then
// type. Caller must hold at least the read lock.
func (s *Server) ratesFor(scenario, timeMonth string) []rateDTO {
	out := []rateDTO{}
	for _, r := range s.state.Meta.Rates.List(scenario, timeMonth) {
		out = append(out, rateDTO{Currency: r.Currency, Type: r.Type, Value: r.Value})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Currency != out[j].Currency {
			return out[i].Currency < out[j].Currency
		}
		return out[i].Type < out[j].Type
	})
	return out
}

func (s *Server) handleRatesGet(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, s.ratesFor(q.Get("scenario"), q.Get("time")))
}

func (s *Server) handleRatesPut(w http.ResponseWriter, r *http.Request) {
	var rates []rateDTO
	if err := decodeJSON(r, &rates); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	q := r.URL.Query()
	scenario, timeMonth := q.Get("scenario"), q.Get("time")

	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.state.Meta
	if d := meta.Dim(model.DimScenario); d == nil || !d.Has(scenario) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown scenario %q", scenario))
		return
	}
	if !model.TimeIsMonth(timeMonth) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("time %q is not a month member", timeMonth))
		return
	}
	for i, rt := range rates {
		if rt.Currency == "" {
			writeError(w, http.StatusBadRequest, fmt.Errorf("rate %d: currency is required", i))
			return
		}
		if rt.Type != model.RateAverage && rt.Type != model.RateClosing {
			writeError(w, http.StatusBadRequest, fmt.Errorf("rate %d: type must be %s or %s, got %q", i, model.RateAverage, model.RateClosing, rt.Type))
			return
		}
		if rt.Value <= 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("rate %d (%s %s): value must be positive, got %v", i, rt.Currency, rt.Type, rt.Value))
			return
		}
	}
	for _, rt := range rates {
		if err := meta.Rates.Set(scenario, timeMonth, rt.Currency, rt.Type, rt.Value); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("set rate %s %s: %w", rt.Currency, rt.Type, err))
			return
		}
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, s.ratesFor(scenario, timeMonth))
}

// formulaDTO is one account formula on the wire.
type formulaDTO struct {
	Account     string            `json:"account"`
	Formula     string            `json:"formula"`
	Dynamic     bool              `json:"dynamic"`
	AccountType model.AccountType `json:"accountType,omitempty"`
}

func (s *Server) handleFormulasGet(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.state.Meta.Account()
	names := make([]string, 0, len(d.Members))
	for name, m := range d.Members {
		if m != nil && m.Formula != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := []formulaDTO{}
	for _, name := range names {
		m := d.Get(name)
		out = append(out, formulaDTO{Account: name, Formula: m.Formula, Dynamic: m.DynamicCalc, AccountType: m.AccountType})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleFormulaPut(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Formula string `json:"formula"`
		Dynamic bool   `json:"dynamic"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	account := r.PathValue("account")
	m := s.state.Meta.Account().Get(account)
	if m == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("account %q not found", account))
		return
	}
	if req.Formula == "" {
		m.Formula = ""
		m.DynamicCalc = false
	} else {
		if _, err := calc.Parse(req.Formula); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !req.Dynamic && !s.state.Meta.Account().IsLeaf(account) {
			writeError(w, http.StatusBadRequest,
				fmt.Errorf("account %q is not a leaf; stored-calc formulas must target leaf accounts (use a dynamic formula instead)", account))
			return
		}
		m.Formula = req.Formula
		m.DynamicCalc = req.Dynamic
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, formulaDTO{Account: account, Formula: m.Formula, Dynamic: m.DynamicCalc, AccountType: m.AccountType})
}
