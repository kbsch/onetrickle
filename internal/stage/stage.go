// Package stage implements staged data import (SPEC §7): import profiles
// describing how delimited files map onto dimensions, transformation rules
// (exact / prefix / default), validation of mapped rows against metadata,
// and grouping of clean rows into a load plan of cell writes.
package stage

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
)

// RuleKind classifies a transformation rule.
type RuleKind string

// Rule kinds.
const (
	KindExact   RuleKind = "exact"   // Src matches the raw value exactly
	KindPrefix  RuleKind = "prefix"  // Src like "41*" matches raw values starting with "41"
	KindDefault RuleKind = "default" // Src "*" matches anything (last resort)
)

// DimIC addresses the implicit intercompany dimension in Profile.Columns,
// Rule.Dim and MappedRow.Raw. IC is not a stored model dimension (it is
// validated against Entity), so model defines no constant for it.
const DimIC model.DimType = "IC"

// Rule maps a raw source value of one dimension to a target member name.
type Rule struct {
	Dim    model.DimType `json:"dim"`
	Kind   RuleKind      `json:"kind"`
	Src    string        `json:"src"`
	Target string        `json:"target"`
}

// ColumnSpec says where a dimension's raw value comes from: a 0-based CSV
// column index, or the Fixed literal when Col is -1 (any negative value).
type ColumnSpec struct {
	Col   int    `json:"col"`
	Fixed string `json:"fixed"`
}

// Profile describes one import format: target cube, CSV shape and the
// transformation rules to apply.
type Profile struct {
	Name      string                       `json:"name"`
	Cube      string                       `json:"cube"`
	HasHeader bool                         `json:"hasHeader"`
	Delimiter string                       `json:"delimiter"` // single character; default ","
	Columns   map[model.DimType]ColumnSpec `json:"columns"`   // Entity, Account, Scenario, Time, Flow, IC, UD1..4
	AmountCol int                          `json:"amountCol"`
	Rules     []Rule                       `json:"rules"`
}

// MappedRow is one source row after column extraction and rule application.
type MappedRow struct {
	Line     int                      `json:"line"`
	Entity   string                   `json:"entity"`
	Account  string                   `json:"account"`
	Scenario string                   `json:"scenario"`
	Time     string                   `json:"time"`
	Flow     string                   `json:"flow"`
	IC       string                   `json:"ic"`
	UD1      string                   `json:"ud1"`
	UD2      string                   `json:"ud2"`
	UD3      string                   `json:"ud3"`
	UD4      string                   `json:"ud4"`
	Amount   float64                  `json:"amount"`
	Raw      map[model.DimType]string `json:"raw"`
	Issues   []string                 `json:"issues"`
}

// TransformResult is the outcome of Transform (optionally enriched by
// Validate). Rows are retained even when flagged with issues so previews can
// show exactly what failed; Issues carries file-level problems and the global
// validation summary.
type TransformResult struct {
	Rows   []*MappedRow `json:"rows"`
	Issues []string     `json:"issues"`

	// cube is the profile's target cube, recorded by Transform so LoadPlan
	// can build unit keys. It does not survive a JSON round trip: call
	// LoadPlan on the in-memory result returned by Transform.
	cube string
}

// profileDims is the fixed set of dimensions a profile may map, in
// presentation order.
var profileDims = []model.DimType{
	model.DimEntity, model.DimAccount, model.DimScenario, model.DimTime,
	model.DimFlow, DimIC, model.DimUD1, model.DimUD2, model.DimUD3, model.DimUD4,
}

// requiredDims must have a ColumnSpec; the remaining dims default to "None".
var requiredDims = []model.DimType{
	model.DimEntity, model.DimAccount, model.DimScenario, model.DimTime,
}

// Transform parses csvData per the profile and applies its transformation
// rules. Structural profile problems (missing required column specs, bad
// delimiter, negative amount column, malformed rules) return an error; data
// problems (bad amounts, short rows) are recorded as row issues with the row
// retained.
func Transform(p *Profile, csvData []byte) (*TransformResult, error) {
	if p == nil {
		return nil, errors.New("transform: nil profile")
	}
	comma, err := delimiterRune(p.Delimiter)
	if err != nil {
		return nil, fmt.Errorf("transform profile %q: %w", p.Name, err)
	}
	if p.AmountCol < 0 {
		return nil, fmt.Errorf("transform profile %q: amount column not set (amountCol=%d)", p.Name, p.AmountCol)
	}
	for _, d := range requiredDims {
		if _, ok := p.Columns[d]; !ok {
			return nil, fmt.Errorf("transform profile %q: missing column spec for required dimension %s", p.Name, d)
		}
	}
	rules, err := indexRules(p.Rules)
	if err != nil {
		return nil, fmt.Errorf("transform profile %q: %w", p.Name, err)
	}

	r := csv.NewReader(bytes.NewReader(csvData))
	r.Comma = comma
	r.FieldsPerRecord = -1 // ragged rows become per-row issues, not reader errors
	r.TrimLeadingSpace = true

	res := &TransformResult{Rows: []*MappedRow{}, cube: p.Cube}
	recNum := 0
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				recNum++
				res.Issues = append(res.Issues, fmt.Sprintf("line %d: csv parse error: %v", pe.Line, pe.Err))
				continue
			}
			return nil, fmt.Errorf("transform profile %q: read csv: %w", p.Name, err)
		}
		recNum++
		if recNum == 1 && p.HasHeader {
			continue
		}
		line, _ := r.FieldPos(0)
		res.Rows = append(res.Rows, transformRecord(p, rules, rec, line))
	}
	return res, nil
}

// transformRecord maps one CSV record to a MappedRow.
func transformRecord(p *Profile, rules map[model.DimType]*dimRules, rec []string, line int) *MappedRow {
	row := &MappedRow{Line: line, Raw: make(map[model.DimType]string, len(profileDims))}
	issue := func(format string, args ...any) {
		row.Issues = append(row.Issues, fmt.Sprintf("line %d: ", line)+fmt.Sprintf(format, args...))
	}
	for _, d := range profileDims {
		raw, ok := rawValue(p, d, rec)
		if !ok {
			issue("missing column %d for %s", p.Columns[d].Col, d)
		}
		row.Raw[d] = raw
		row.setDim(d, applyRules(rules[d], raw))
	}
	if p.AmountCol >= len(rec) {
		issue("missing amount column %d", p.AmountCol)
		return row
	}
	amt := strings.TrimSpace(rec[p.AmountCol])
	v, err := strconv.ParseFloat(amt, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		// Non-finite literals ("NaN", "Inf", …) parse without error but would
		// poison JSON persistence; they are bad amounts like any other.
		issue("bad amount %q", amt)
		return row
	}
	row.Amount = v
	return row
}

// rawValue extracts the raw (pre-rule) value of a dimension from a record.
// ok is false when the spec points at a column the record does not have.
func rawValue(p *Profile, d model.DimType, rec []string) (raw string, ok bool) {
	spec, has := p.Columns[d]
	if !has {
		// Required dims are checked up front in Transform; the rest default.
		return model.NoneMember, true
	}
	if spec.Col < 0 {
		return strings.TrimSpace(spec.Fixed), true
	}
	if spec.Col >= len(rec) {
		return "", false
	}
	return strings.TrimSpace(rec[spec.Col]), true
}

// setDim stores a mapped value into the named field for the dimension.
func (r *MappedRow) setDim(d model.DimType, v string) {
	switch d {
	case model.DimEntity:
		r.Entity = v
	case model.DimAccount:
		r.Account = v
	case model.DimScenario:
		r.Scenario = v
	case model.DimTime:
		r.Time = v
	case model.DimFlow:
		r.Flow = v
	case DimIC:
		r.IC = v
	case model.DimUD1:
		r.UD1 = v
	case model.DimUD2:
		r.UD2 = v
	case model.DimUD3:
		r.UD3 = v
	case model.DimUD4:
		r.UD4 = v
	}
}

// dimRules is the indexed rule set of one dimension.
type dimRules struct {
	exact  map[string]string
	prefix []prefixRule
	def    string
	hasDef bool
}

type prefixRule struct {
	prefix string
	target string
}

// indexRules groups and classifies a profile's rules per dimension. A rule
// with an empty Kind is inferred from Src ("*" → default, trailing "*" →
// prefix, else exact). Unknown kinds are an error.
func indexRules(rules []Rule) (map[model.DimType]*dimRules, error) {
	out := map[model.DimType]*dimRules{}
	for i, r := range rules {
		kind := r.Kind
		if kind == "" {
			switch {
			case r.Src == "*":
				kind = KindDefault
			case strings.HasSuffix(r.Src, "*"):
				kind = KindPrefix
			default:
				kind = KindExact
			}
		}
		dr := out[r.Dim]
		if dr == nil {
			dr = &dimRules{exact: map[string]string{}}
			out[r.Dim] = dr
		}
		switch kind {
		case KindExact:
			dr.exact[r.Src] = r.Target
		case KindPrefix:
			dr.prefix = append(dr.prefix, prefixRule{prefix: strings.TrimSuffix(r.Src, "*"), target: r.Target})
		case KindDefault:
			dr.def = r.Target
			dr.hasDef = true
		default:
			return nil, fmt.Errorf("rule %d (%s %q): unknown rule kind %q", i, r.Dim, r.Src, r.Kind)
		}
	}
	return out, nil
}

// applyRules resolves a raw value: exact match first, then the longest
// matching prefix (later rules win length ties), then the default rule, and
// finally identity (the raw value unchanged — mapping is optional).
func applyRules(dr *dimRules, raw string) string {
	if dr == nil {
		return raw
	}
	if t, ok := dr.exact[raw]; ok {
		return t
	}
	best := -1
	target := ""
	for _, pr := range dr.prefix {
		if strings.HasPrefix(raw, pr.prefix) && len(pr.prefix) >= best {
			best = len(pr.prefix)
			target = pr.target
		}
	}
	if best >= 0 {
		return target
	}
	if dr.hasDef {
		return dr.def
	}
	return raw
}

// delimiterRune validates and decodes a profile delimiter ("" → comma).
func delimiterRune(s string) (rune, error) {
	if s == "" {
		return ',', nil
	}
	r, size := utf8.DecodeRuneInString(s)
	if size != len(s) {
		return 0, fmt.Errorf("delimiter %q must be a single character", s)
	}
	if r == utf8.RuneError || r == '"' || r == '\r' || r == '\n' {
		return 0, fmt.Errorf("invalid delimiter %q", s)
	}
	return r, nil
}

// leafCheck pairs a mapped dimension value with the dimension that must
// contain it as a leaf member.
type leafCheck struct {
	dim model.DimType
	val string
}

// Validate checks every mapped row against the metadata, appending issues in
// place: members must exist and be leaves in their dimension, Time must be an
// existing month, IC must be a valid intercompany partner, and accounts with
// formulas or DynamicCalc reject loads. A summary of distinct problems (with
// row counts) is appended to res.Issues, sorted for determinism.
func Validate(meta *model.Metadata, res *TransformResult) {
	if meta == nil || res == nil {
		return
	}
	summary := map[string]int{}
	for _, row := range res.Rows {
		if row == nil {
			continue
		}
		add := func(format string, args ...any) {
			msg := fmt.Sprintf(format, args...)
			row.Issues = append(row.Issues, fmt.Sprintf("line %d: %s", row.Line, msg))
			summary[msg]++
		}
		checks := []leafCheck{
			{model.DimEntity, row.Entity},
			{model.DimAccount, row.Account},
			{model.DimScenario, row.Scenario},
			{model.DimFlow, row.Flow},
			{model.DimUD1, row.UD1},
			{model.DimUD2, row.UD2},
			{model.DimUD3, row.UD3},
			{model.DimUD4, row.UD4},
		}
		for _, c := range checks {
			d := meta.Dim(c.dim)
			if d == nil || !d.Has(c.val) {
				add("unknown %s %q", c.dim, c.val)
				continue
			}
			if !d.IsLeaf(c.val) {
				add("%s %q is not a leaf", c.dim, c.val)
			}
		}
		if a := meta.Account().Get(row.Account); a != nil {
			if a.DynamicCalc {
				add("Account %q is a dynamic-calc account; cannot load", row.Account)
			} else if a.Formula != "" {
				add("Account %q has a formula; cannot load", row.Account)
			}
		}
		if !model.TimeIsMonth(row.Time) {
			add("Time %q is not a month", row.Time)
		} else if d := meta.Dim(model.DimTime); d == nil || !d.Has(row.Time) {
			add("unknown Time %q", row.Time)
		}
		if !meta.ValidIC(row.IC) {
			add("invalid IC partner %q", row.IC)
		}
	}
	msgs := make([]string, 0, len(summary))
	for m := range summary {
		msgs = append(msgs, m)
	}
	sort.Strings(msgs)
	for _, m := range msgs {
		n := summary[m]
		unit := "rows"
		if n == 1 {
			unit = "row"
		}
		res.Issues = append(res.Issues, fmt.Sprintf("%s (%d %s)", m, n, unit))
	}
}

// LoadPlan groups the clean rows (no issues) of a transform result into cell
// writes per data unit. Coordinates get Origin=Import and are normalized
// (empty Flow/IC/UD → "None"); duplicate coordinates within a unit sum their
// amounts. Writes within a unit are sorted by coordinate key for determinism.
func LoadPlan(res *TransformResult) map[cube.UnitKey][]cube.CellWrite {
	plan := map[cube.UnitKey][]cube.CellWrite{}
	if res == nil {
		return plan
	}
	sums := map[cube.UnitKey]map[cube.CellCoord]float64{}
	for _, row := range res.Rows {
		if row == nil || len(row.Issues) > 0 {
			continue
		}
		uk := cube.UnitKey{Cube: res.cube, Entity: row.Entity, Scenario: row.Scenario, Time: row.Time}
		coord := cube.CellCoord{
			Account: row.Account,
			Flow:    row.Flow,
			Origin:  model.OriginImport,
			IC:      row.IC,
			UD1:     row.UD1,
			UD2:     row.UD2,
			UD3:     row.UD3,
			UD4:     row.UD4,
		}.Normalize()
		if sums[uk] == nil {
			sums[uk] = map[cube.CellCoord]float64{}
		}
		sums[uk][coord] += row.Amount
	}
	for uk, cells := range sums {
		writes := make([]cube.CellWrite, 0, len(cells))
		for c, v := range cells {
			writes = append(writes, cube.CellWrite{Unit: uk, Coord: c, Value: v})
		}
		sort.Slice(writes, func(i, j int) bool { return writes[i].Coord.Key() < writes[j].Coord.Key() })
		plan[uk] = writes
	}
	return plan
}
