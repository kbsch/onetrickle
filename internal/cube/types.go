// Package cube stores cell data and answers dimensional queries.
// This file (types, key codecs, store container) is hand-authored and shared;
// the query engine lives in engine.go.
package cube

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"onetrickle/internal/model"
)

// UnitKey addresses one data unit. Time is always a month member.
type UnitKey struct {
	Cube     string `json:"cube"`
	Entity   string `json:"entity"`
	Scenario string `json:"scenario"`
	Time     string `json:"time"`
}

// Key encodes the unit key as "cube|entity|scenario|time".
func (u UnitKey) Key() string {
	return strings.Join([]string{u.Cube, u.Entity, u.Scenario, u.Time}, "|")
}

// ParseUnitKey reverses UnitKey.Key.
func ParseUnitKey(s string) (UnitKey, error) {
	p := strings.Split(s, "|")
	if len(p) != 4 {
		return UnitKey{}, fmt.Errorf("bad unit key %q", s)
	}
	return UnitKey{p[0], p[1], p[2], p[3]}, nil
}

// CellCoord addresses one cell within a data unit.
type CellCoord struct {
	Account string `json:"account"`
	Flow    string `json:"flow"`
	Origin  string `json:"origin"`
	IC      string `json:"ic"`
	UD1     string `json:"ud1"`
	UD2     string `json:"ud2"`
	UD3     string `json:"ud3"`
	UD4     string `json:"ud4"`
}

// Normalize fills empty Flow/IC/UD fields with model.NoneMember.
func (c CellCoord) Normalize() CellCoord {
	def := func(s string) string {
		if s == "" {
			return model.NoneMember
		}
		return s
	}
	c.Flow, c.IC = def(c.Flow), def(c.IC)
	c.UD1, c.UD2, c.UD3, c.UD4 = def(c.UD1), def(c.UD2), def(c.UD3), def(c.UD4)
	return c
}

// Key encodes the coordinate as "acct|flow|origin|ic|ud1|ud2|ud3|ud4".
func (c CellCoord) Key() string {
	return strings.Join([]string{c.Account, c.Flow, c.Origin, c.IC, c.UD1, c.UD2, c.UD3, c.UD4}, "|")
}

// ParseCellCoord reverses CellCoord.Key.
func ParseCellCoord(s string) (CellCoord, error) {
	p := strings.Split(s, "|")
	if len(p) != 8 {
		return CellCoord{}, fmt.Errorf("bad cell coord %q", s)
	}
	return CellCoord{p[0], p[1], p[2], p[3], p[4], p[5], p[6], p[7]}, nil
}

// CellMap holds cells of one stage. It marshals to JSON with string keys.
type CellMap map[CellCoord]float64

// MarshalJSON encodes the map keyed by CellCoord.Key().
func (m CellMap) MarshalJSON() ([]byte, error) {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k.Key()] = v
	}
	return json.Marshal(out)
}

// UnmarshalJSON reverses MarshalJSON.
func (m *CellMap) UnmarshalJSON(b []byte) error {
	var raw map[string]float64
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(CellMap, len(raw))
	for k, v := range raw {
		c, err := ParseCellCoord(k)
		if err != nil {
			return err
		}
		out[c] = v
	}
	*m = out
	return nil
}

// Unit holds all cells of one data unit. Input is the Local stage
// (Origin ∈ {Import, Forms, Adj, Calc}); Stages holds engine-materialized
// Translated / Elimination / Consolidated cells.
type Unit struct {
	Input  CellMap                     `json:"input,omitempty"`
	Stages map[model.ConsStage]CellMap `json:"stages,omitempty"`
}

// NewUnit returns an empty unit.
func NewUnit() *Unit {
	return &Unit{Input: CellMap{}, Stages: map[model.ConsStage]CellMap{}}
}

// Stage returns the cell map for a stage, creating it if needed.
// StageLocal returns Input.
func (u *Unit) Stage(s model.ConsStage) CellMap {
	if s == model.StageLocal {
		if u.Input == nil {
			u.Input = CellMap{}
		}
		return u.Input
	}
	if u.Stages == nil {
		u.Stages = map[model.ConsStage]CellMap{}
	}
	cm := u.Stages[s]
	if cm == nil {
		cm = CellMap{}
		u.Stages[s] = cm
	}
	return cm
}

// ClearStage drops all cells of a materialized stage (no-op for Local).
func (u *Unit) ClearStage(s model.ConsStage) {
	if s == model.StageLocal {
		return
	}
	delete(u.Stages, s)
}

// ClearOrigin removes all Input cells with the given origin.
func (u *Unit) ClearOrigin(origin string) {
	for c := range u.Input {
		if c.Origin == origin {
			delete(u.Input, c)
		}
	}
}

// Store is the in-memory cell database. The server serializes access with its
// own lock; Store's mutex additionally protects map structure for safety.
type Store struct {
	mu    sync.RWMutex
	Units map[UnitKey]*Unit
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{Units: map[UnitKey]*Unit{}} }

// Unit returns the unit or nil.
func (s *Store) Unit(k UnitKey) *Unit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Units[k]
}

// Ensure returns the unit, creating it if absent.
func (s *Store) Ensure(k UnitKey) *Unit {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.Units[k]
	if u == nil {
		u = NewUnit()
		s.Units[k] = u
	}
	return u
}

// Delete removes a unit.
func (s *Store) Delete(k UnitKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.Units, k)
}

// Keys returns all unit keys, optionally filtered (empty field = any).
func (s *Store) Keys(filter UnitKey) []UnitKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []UnitKey
	for k := range s.Units {
		if (filter.Cube == "" || k.Cube == filter.Cube) &&
			(filter.Entity == "" || k.Entity == filter.Entity) &&
			(filter.Scenario == "" || k.Scenario == filter.Scenario) &&
			(filter.Time == "" || k.Time == filter.Time) {
			out = append(out, k)
		}
	}
	return out
}

// CellWrite is one user-facing cell mutation (value 0 deletes).
type CellWrite struct {
	Unit  UnitKey   `json:"unit"`
	Coord CellCoord `json:"coord"`
	Value float64   `json:"value"`
}

// MarshalJSON / UnmarshalJSON give Store a stable JSON shape
// {"<unitkey>": <unit>} for persistence.
func (s *Store) MarshalJSON() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]*Unit, len(s.Units))
	for k, u := range s.Units {
		out[k.Key()] = u
	}
	return json.Marshal(out)
}

func (s *Store) UnmarshalJSON(b []byte) error {
	var raw map[string]*Unit
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	units := make(map[UnitKey]*Unit, len(raw))
	for k, u := range raw {
		uk, err := ParseUnitKey(k)
		if err != nil {
			return err
		}
		if u.Input == nil {
			u.Input = CellMap{}
		}
		units[uk] = u
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Units = units
	return nil
}
