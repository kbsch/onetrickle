package model

import (
	"fmt"
	"sort"
	"time"
)

// Cube binds the shared dimensions to a reporting (group) currency.
type Cube struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	GroupCurrency string `json:"groupCurrency"`
}

// Metadata is the full dimensional model of the application.
type Metadata struct {
	Cubes map[string]*Cube       `json:"cubes"`
	Dims  map[DimType]*Dimension `json:"dims"`
	Rates RateTable              `json:"rates"`
}

// NewMetadata builds an empty but structurally valid model: fixed Origin
// members, None roots for Flow/UD dims, a Time dimension seeded with the
// current year, and no cubes/scenarios/entities/accounts.
func NewMetadata() *Metadata {
	m := &Metadata{
		Cubes: map[string]*Cube{},
		Dims:  map[DimType]*Dimension{},
		Rates: RateTable{},
	}
	for _, t := range AllDims {
		m.Dims[t] = NewDimension(t)
	}
	for _, o := range []string{OriginImport, OriginForms, OriginAdj, OriginCalc, OriginElim} {
		_ = m.Dims[DimOrigin].AddMember(&Member{Name: o})
	}
	for _, t := range []DimType{DimFlow, DimUD1, DimUD2, DimUD3, DimUD4} {
		_ = m.Dims[t].AddMember(&Member{Name: NoneMember})
	}
	m.Dims[DimTime] = BuildTimeDim([]int{time.Now().Year()})
	return m
}

// Dim returns the dimension of the given type (nil if unknown).
func (m *Metadata) Dim(t DimType) *Dimension { return m.Dims[t] }

// Entity, Account are convenience accessors for the two most used dims.
func (m *Metadata) Entity() *Dimension  { return m.Dims[DimEntity] }
func (m *Metadata) Account() *Dimension { return m.Dims[DimAccount] }

// CubeOf returns the cube or an error.
func (m *Metadata) CubeOf(name string) (*Cube, error) {
	c := m.Cubes[name]
	if c == nil {
		return nil, fmt.Errorf("cube %q not found", name)
	}
	return c, nil
}

// EntityCurrency returns the currency of an entity, defaulting to the cube
// group currency when unset.
func (m *Metadata) EntityCurrency(cube *Cube, entity string) string {
	if e := m.Entity().Get(entity); e != nil && e.Currency != "" {
		return e.Currency
	}
	return cube.GroupCurrency
}

// AccountType returns the type of an account member, defaulting to Revenue
// for unset types so unconfigured accounts behave as flows.
func (m *Metadata) AccountType(account string) AccountType {
	if a := m.Account().Get(account); a != nil && a.AccountType.Valid() {
		return a.AccountType
	}
	return AccountRevenue
}

// ValidIC reports whether an IC partner value is valid: NoneMember or an
// existing entity.
func (m *Metadata) ValidIC(ic string) bool {
	return ic == NoneMember || ic == "" || m.Entity().Has(ic)
}

// Currencies returns every currency referenced by entities or cubes, sorted.
func (m *Metadata) Currencies() []string {
	set := map[string]bool{}
	for _, c := range m.Cubes {
		if c.GroupCurrency != "" {
			set[c.GroupCurrency] = true
		}
	}
	for _, e := range m.Entity().Members {
		if e.Currency != "" {
			set[e.Currency] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Validate aggregates structural problems across all dimensions plus
// cross-dimension rules (entity currencies, IC flags).
func (m *Metadata) Validate() []string {
	var probs []string
	for _, t := range AllDims {
		d := m.Dims[t]
		if d == nil {
			probs = append(probs, fmt.Sprintf("missing dimension %s", t))
			continue
		}
		probs = append(probs, d.Validate()...)
	}
	for name, c := range m.Cubes {
		if c.GroupCurrency == "" {
			probs = append(probs, fmt.Sprintf("cube %q has no group currency", name))
		}
	}
	// Flow and UD dimensions must keep their mandatory None root (SPEC §2):
	// stored cells normalize empty coordinates to None.
	for _, t := range []DimType{DimFlow, DimUD1, DimUD2, DimUD3, DimUD4} {
		d := m.Dims[t]
		if d == nil {
			continue
		}
		if nm := d.Get(NoneMember); nm == nil {
			probs = append(probs, fmt.Sprintf("%s: missing mandatory root member %q", t, NoneMember))
		} else if nm.Parent != "" {
			probs = append(probs, fmt.Sprintf("%s: member %q must be a root", t, NoneMember))
		}
	}
	return probs
}
