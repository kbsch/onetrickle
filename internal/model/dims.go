// Package model defines the dimensional metadata of onetrickle: dimensions,
// members, hierarchies, account semantics, time, and FX rates. It has no
// dependencies on other onetrickle packages.
package model

import (
	"fmt"
	"regexp"
	"slices"
)

// DimType identifies one of the fixed set of dimensions.
type DimType string

const (
	DimEntity   DimType = "Entity"
	DimAccount  DimType = "Account"
	DimScenario DimType = "Scenario"
	DimTime     DimType = "Time"
	DimFlow     DimType = "Flow"
	DimOrigin   DimType = "Origin"
	DimUD1      DimType = "UD1"
	DimUD2      DimType = "UD2"
	DimUD3      DimType = "UD3"
	DimUD4      DimType = "UD4"
)

// AllDims lists every dimension that is stored as a Dimension value.
// View and IC are implicit (View is fixed Periodic/YTD; IC references Entity).
var AllDims = []DimType{
	DimEntity, DimAccount, DimScenario, DimTime, DimFlow, DimOrigin,
	DimUD1, DimUD2, DimUD3, DimUD4,
}

// UserEditableDims are the dimensions whose members users may manage.
var UserEditableDims = []DimType{
	DimEntity, DimAccount, DimScenario, DimFlow, DimUD1, DimUD2, DimUD3, DimUD4,
}

// View members (fixed, never stored).
const (
	ViewPeriodic = "Periodic"
	ViewYTD      = "YTD"
)

// ConsStage is a consolidation stage of a data unit.
type ConsStage string

const (
	StageLocal        ConsStage = "Local"
	StageTranslated   ConsStage = "Translated"
	StageElimination  ConsStage = "Elimination"
	StageConsolidated ConsStage = "Consolidated"
)

// Fixed Origin members.
const (
	OriginImport = "Import"
	OriginForms  = "Forms"
	OriginAdj    = "Adj"
	OriginCalc   = "Calc" // engine-written: stored calc results
	OriginElim   = "Elim" // engine-written: elimination entries (stage maps only)
)

// UserOrigins are the origins user writes may target.
var UserOrigins = []string{OriginImport, OriginForms, OriginAdj}

// NoneMember is the default member name for Flow, IC and UD dimensions.
const NoneMember = "None"

var memberNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._\-]*$`)

// ValidMemberName reports whether s is a legal member name. Beyond the
// character regex, a name must round-trip as a calc A# formula reference
// (SPEC §6): it may not end in a space, and every hyphen must be tightly
// bound between name-word characters (letters, digits, '.' or '_') — no
// leading, trailing or space-adjacent hyphen. Otherwise the A# ref lexer
// would read a different or truncated name, leaving a valid member that no
// formula could reference.
func ValidMemberName(s string) error {
	if !memberNameRE.MatchString(s) {
		return fmt.Errorf("invalid member name %q: must match %s", s, memberNameRE.String())
	}
	// The leading-[A-Za-z0-9] anchor already guarantees s is non-empty and
	// starts with a name-word char (so no leading hyphen/space).
	if s[len(s)-1] == ' ' {
		return fmt.Errorf("invalid member name %q: must not end with a space", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] != '-' {
			continue
		}
		if i == 0 || i == len(s)-1 || s[i-1] == ' ' || s[i+1] == ' ' {
			return fmt.Errorf("invalid member name %q: a hyphen must sit between letters, digits, '.' or '_' so the name is usable as a calc A# reference", s)
		}
	}
	return nil
}

// Member is a node in a dimension hierarchy. Weight, AccountType, IsIC,
// DynamicCalc, Formula apply to Account members; Currency and OwnershipPct
// apply to Entity members. Single-parent hierarchies only.
type Member struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Parent      string   `json:"parent,omitempty"` // "" for roots
	Children    []string `json:"children,omitempty"`
	Weight      float64  `json:"weight"` // aggregation weight into Parent; default 1

	// Account properties.
	AccountType AccountType `json:"accountType,omitempty"`
	IsIC        bool        `json:"isIC,omitempty"`
	DynamicCalc bool        `json:"dynamicCalc,omitempty"`
	Formula     string      `json:"formula,omitempty"`

	// Entity properties.
	Currency     string  `json:"currency,omitempty"`
	OwnershipPct float64 `json:"ownershipPct,omitempty"` // % consolidated into Parent; default 100
}

// Dimension is a named hierarchy of members.
type Dimension struct {
	Type    DimType            `json:"type"`
	Members map[string]*Member `json:"members"`
	Roots   []string           `json:"roots"`
}

// NewDimension returns an empty dimension of the given type.
func NewDimension(t DimType) *Dimension {
	return &Dimension{Type: t, Members: map[string]*Member{}}
}

// Get returns the member or nil.
func (d *Dimension) Get(name string) *Member { return d.Members[name] }

// Has reports whether the member exists.
func (d *Dimension) Has(name string) bool { _, ok := d.Members[name]; return ok }

// AddMember inserts m under m.Parent ("" = root). Defaults: Weight 1,
// OwnershipPct 100 (entities).
func (d *Dimension) AddMember(m *Member) error {
	if err := ValidMemberName(m.Name); err != nil {
		return err
	}
	if d.Has(m.Name) {
		return fmt.Errorf("member %q already exists in %s", m.Name, d.Type)
	}
	if m.Parent != "" && !d.Has(m.Parent) {
		return fmt.Errorf("parent %q not found in %s", m.Parent, d.Type)
	}
	if m.Weight == 0 {
		m.Weight = 1
	}
	if d.Type == DimEntity && m.OwnershipPct == 0 {
		m.OwnershipPct = 100
	}
	m.Children = nil
	d.Members[m.Name] = m
	if m.Parent == "" {
		d.Roots = append(d.Roots, m.Name)
	} else {
		p := d.Members[m.Parent]
		p.Children = append(p.Children, m.Name)
	}
	return nil
}

// RemoveMember deletes a member. If recursive is false and the member has
// children, it fails.
func (d *Dimension) RemoveMember(name string, recursive bool) error {
	m := d.Get(name)
	if m == nil {
		return fmt.Errorf("member %q not found in %s", name, d.Type)
	}
	if len(m.Children) > 0 && !recursive {
		return fmt.Errorf("member %q has children; use recursive delete", name)
	}
	for _, c := range slices.Clone(m.Children) {
		if err := d.RemoveMember(c, true); err != nil {
			return err
		}
	}
	if m.Parent == "" {
		d.Roots = slices.DeleteFunc(slices.Clone(d.Roots), func(s string) bool { return s == name })
	} else if p := d.Get(m.Parent); p != nil {
		p.Children = slices.DeleteFunc(slices.Clone(p.Children), func(s string) bool { return s == name })
	}
	delete(d.Members, name)
	return nil
}

// MoveMember reparents a member (newParent "" = make root). Fails on cycles.
func (d *Dimension) MoveMember(name, newParent string) error {
	m := d.Get(name)
	if m == nil {
		return fmt.Errorf("member %q not found in %s", name, d.Type)
	}
	if newParent != "" {
		if !d.Has(newParent) {
			return fmt.Errorf("parent %q not found in %s", newParent, d.Type)
		}
		if name == newParent || d.IsAncestor(name, newParent) {
			return fmt.Errorf("moving %q under %q would create a cycle", name, newParent)
		}
	}
	if m.Parent == "" {
		d.Roots = slices.DeleteFunc(slices.Clone(d.Roots), func(s string) bool { return s == name })
	} else if p := d.Get(m.Parent); p != nil {
		p.Children = slices.DeleteFunc(slices.Clone(p.Children), func(s string) bool { return s == name })
	}
	m.Parent = newParent
	if newParent == "" {
		d.Roots = append(d.Roots, name)
	} else {
		np := d.Get(newParent)
		np.Children = append(np.Children, name)
	}
	return nil
}

// IsLeaf reports whether the member exists and has no children.
func (d *Dimension) IsLeaf(name string) bool {
	m := d.Get(name)
	return m != nil && len(m.Children) == 0
}

// Descendants returns all members strictly below name, pre-order.
func (d *Dimension) Descendants(name string) []string {
	m := d.Get(name)
	if m == nil {
		return nil
	}
	var out []string
	for _, c := range m.Children {
		out = append(out, c)
		out = append(out, d.Descendants(c)...)
	}
	return out
}

// Leaves returns the leaf members at or below name (name itself if leaf).
func (d *Dimension) Leaves(name string) []string {
	if d.IsLeaf(name) {
		return []string{name}
	}
	m := d.Get(name)
	if m == nil {
		return nil
	}
	var out []string
	for _, c := range m.Children {
		out = append(out, d.Leaves(c)...)
	}
	return out
}

// PathToRoot returns name, parent, ..., root.
func (d *Dimension) PathToRoot(name string) []string {
	var out []string
	for cur := name; cur != ""; {
		m := d.Get(cur)
		if m == nil {
			return out
		}
		out = append(out, cur)
		cur = m.Parent
	}
	return out
}

// IsAncestor reports whether anc is a strict ancestor of name.
func (d *Dimension) IsAncestor(anc, name string) bool {
	m := d.Get(name)
	for m != nil && m.Parent != "" {
		if m.Parent == anc {
			return true
		}
		m = d.Get(m.Parent)
	}
	return false
}

// FCA returns the first (deepest) common ancestor of a and b, including the
// case where one is an ancestor of the other ("" if none).
func (d *Dimension) FCA(a, b string) string {
	if a == b {
		return a
	}
	onPathA := map[string]bool{}
	for _, n := range d.PathToRoot(a) {
		onPathA[n] = true
	}
	for _, n := range d.PathToRoot(b) {
		if onPathA[n] {
			return n
		}
	}
	return ""
}

// Validate returns human-readable structural problems (broken parent links,
// orphaned children references, bad names).
func (d *Dimension) Validate() []string {
	var probs []string
	for name, m := range d.Members {
		if err := ValidMemberName(name); err != nil {
			probs = append(probs, err.Error())
		}
		if m.Parent != "" && !d.Has(m.Parent) {
			probs = append(probs, fmt.Sprintf("%s: member %q has missing parent %q", d.Type, name, m.Parent))
		}
		for _, c := range m.Children {
			cm := d.Get(c)
			if cm == nil {
				probs = append(probs, fmt.Sprintf("%s: member %q lists missing child %q", d.Type, name, c))
			} else if cm.Parent != name {
				probs = append(probs, fmt.Sprintf("%s: child %q of %q has parent %q", d.Type, c, name, cm.Parent))
			}
		}
	}
	for _, r := range d.Roots {
		if !d.Has(r) {
			probs = append(probs, fmt.Sprintf("%s: missing root %q", d.Type, r))
		}
	}
	return probs
}
