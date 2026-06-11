// Package consol implements the consolidation engine (SPEC §5): per-unit
// stored calculations, FX translation into the cube group currency,
// intercompany eliminations posted at the first common ancestor, and
// ownership-weighted consolidation over the entity tree — materialized for
// one (cube, scenario, month) slice.
package consol

import (
	"fmt"
	"math"
	"sort"

	"onetrickle/internal/calc"
	"onetrickle/internal/cube"
	"onetrickle/internal/model"
)

// eps is the write threshold: computed cells with |value| below it are
// skipped (never stored).
const eps = 1e-12

// Result reports one Process run: the identity of the processed slice,
// counts of touched units and written cells, and any non-fatal issues
// (missing FX rates, IC partners with no common ancestor). Issues is never
// nil so it serializes as [].
type Result struct {
	Cube           string   `json:"cube"`
	Scenario       string   `json:"scenario"`
	Time           string   `json:"time"`
	UnitsProcessed int      `json:"unitsProcessed"`
	CellsWritten   int      `json:"cellsWritten"`
	Issues         []string `json:"issues"`
}

// Process materializes the full consolidation for one (cube, scenario,
// month): stored calcs (Origin=Calc), the Translated stage, the Elimination
// stage and the Consolidated stage are each cleared and rebuilt, so
// re-running Process is idempotent. It returns an error for an unknown cube,
// a non-month time member, an empty scenario, an unparsable stored formula
// or a formula dependency cycle; data-level problems (missing rates, IC
// partners without a common ancestor) are recorded as Result.Issues.
func Process(meta *model.Metadata, store *cube.Store, cubeName, scenario, month string) (*Result, error) {
	if meta == nil || store == nil {
		return nil, fmt.Errorf("consol: nil metadata or store")
	}
	cb, err := meta.CubeOf(cubeName)
	if err != nil {
		return nil, fmt.Errorf("consol: %w", err)
	}
	if scenario == "" {
		return nil, fmt.Errorf("consol: scenario is required")
	}
	if !model.TimeIsMonth(month) {
		return nil, fmt.Errorf("consol: time %q is not a month member", month)
	}
	if meta.Entity() == nil || meta.Account() == nil {
		return nil, fmt.Errorf("consol: metadata is missing the Entity or Account dimension")
	}

	res := &Result{Cube: cubeName, Scenario: scenario, Time: month, Issues: []string{}}

	// Units of this slice, in deterministic order. Every existing unit is
	// touched by steps 1-2 (clear + rebuild); units created later (FCA and
	// parent entities) are added to touched as they are ensured.
	keys := store.Keys(cube.UnitKey{Cube: cubeName, Scenario: scenario, Time: month})
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key() < keys[j].Key() })
	touched := make(map[cube.UnitKey]bool, len(keys))
	for _, k := range keys {
		touched[k] = true
	}

	if err := runStoredCalcs(meta, store, keys, res); err != nil {
		return nil, err
	}
	translate(meta, cb, store, keys, scenario, month, res)
	eliminate(meta, store, keys, cubeName, scenario, month, res, touched)
	consolidate(meta, store, cubeName, scenario, month, res, touched)

	res.UnitsProcessed = len(touched)
	return res, nil
}

// runStoredCalcs is step 1: per entity unit, clear Origin=Calc cells and
// re-evaluate every non-dynamic account formula in topological order of its
// A# references. Stored-calc formulas must target LEAF accounts (a Calc cell
// at a non-leaf would double-count with its children in every rollup); a
// non-leaf target is an error. Because a ref resolves as the weighted rollup
// of the referenced account's whole subtree, a ref to an ANCESTOR of another
// formula account is an implicit dependency on that formula: the dependency
// graph is expanded accordingly before ordering, so hidden ancestor cycles
// are reported as cycle errors instead of evaluating in the wrong order.
// For each formula the rest-tuple universe (CellCoord minus Account, Origin
// collapsed) is the union over the formula's refs of tuples where the
// referenced account subtree has any local data; a ref's value is the
// weighted rollup of that subtree at the tuple, summed across origins.
// Results land at (Account=target, Origin=Calc, rest=tuple); |v| < eps is
// skipped and a non-finite result is recorded as an Issue. Because results
// are written into Input before the next formula runs, chained formulas see
// the freshly computed Calc cells.
func runStoredCalcs(meta *model.Metadata, store *cube.Store, keys []cube.UnitKey, res *Result) error {
	acctDim := meta.Account()

	formulas := map[string]*calc.Expr{}
	names := make([]string, 0, len(acctDim.Members))
	for n := range acctDim.Members {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		m := acctDim.Members[n]
		if m.DynamicCalc || m.Formula == "" {
			continue
		}
		if !acctDim.IsLeaf(n) {
			return fmt.Errorf("consol: account %q has a stored-calc formula but is not a leaf; stored calcs must target leaf accounts", n)
		}
		e, err := calc.Parse(m.Formula)
		if err != nil {
			return fmt.Errorf("consol: account %q formula: %w", n, err)
		}
		formulas[n] = e
	}

	// Account-subtree weight maps, shared across units.
	weights := map[string]map[string]float64{}
	weightOf := func(account string) map[string]float64 {
		w, ok := weights[account]
		if !ok {
			w = subtreeWeights(acctDim, account)
			weights[account] = w
		}
		return w
	}

	// Expanded dependency graph: a formula depends on every formula account
	// contained in any of its refs' subtrees (the ref itself or a descendant).
	deps := make(map[string][]string, len(formulas))
	for target, expr := range formulas {
		var ds []string
		for _, r := range expr.Refs() {
			w := weightOf(r)
			for f := range formulas {
				if _, ok := w[f]; ok {
					ds = append(ds, f)
				}
			}
		}
		deps[target] = ds
	}
	order, err := calc.TopoSortDeps(deps)
	if err != nil {
		return fmt.Errorf("consol: %w", err)
	}

	for _, k := range keys {
		u := store.Unit(k)
		if u == nil {
			continue
		}
		u.ClearOrigin(model.OriginCalc)
		if len(order) == 0 || len(u.Input) == 0 {
			continue
		}
		for _, target := range order {
			expr := formulas[target]
			byTuple := groupByTuple(u.Input)
			for _, rt := range tupleUniverse(byTuple, expr.Refs(), weightOf) {
				accounts := byTuple[rt]
				v, err := expr.Eval(func(ref string) (float64, error) {
					return rollup(accounts, weightOf(ref)), nil
				})
				if err != nil {
					return fmt.Errorf("consol: evaluate %q for unit %s: %w", target, k.Key(), err)
				}
				if math.IsNaN(v) || math.IsInf(v, 0) {
					res.Issues = append(res.Issues,
						fmt.Sprintf("formula %q produced a non-finite value for unit %s; cell skipped", target, k.Key()))
					continue
				}
				if math.Abs(v) < eps {
					continue
				}
				coord := rt
				coord.Account = target
				coord.Origin = model.OriginCalc
				u.Input[coord] = v
				res.CellsWritten++
			}
		}
	}
	return nil
}

// translate is step 2: per entity unit, clear and rebuild Stages[Translated]
// from every Input cell multiplied by the FX rate from the entity currency
// into the cube group currency (rate type per the account's AccountType).
// A missing rate is recorded once per (currency, rate type) and falls back
// to 1.0.
func translate(meta *model.Metadata, cb *model.Cube, store *cube.Store, keys []cube.UnitKey, scenario, month string, res *Result) {
	missing := map[string]bool{}
	for _, k := range keys {
		u := store.Unit(k)
		if u == nil {
			continue
		}
		u.ClearStage(model.StageTranslated)
		if len(u.Input) == 0 {
			continue
		}
		currency := meta.EntityCurrency(cb, k.Entity)
		var out cube.CellMap
		for _, c := range sortedCoords(u.Input) {
			rt := meta.AccountType(c.Account).RateType()
			rate, ok := meta.Rates.ToGroup(scenario, month, currency, cb.GroupCurrency, rt)
			if !ok {
				mk := currency + "|" + string(rt)
				if !missing[mk] {
					missing[mk] = true
					res.Issues = append(res.Issues,
						fmt.Sprintf("missing %s rate for %s %s/%s", rt, currency, scenario, month))
				}
				rate = 1
			}
			v := u.Input[c] * rate
			if !writableValue(v, "translate", k, res) || math.Abs(v) < eps {
				continue
			}
			if out == nil {
				out = u.Stage(model.StageTranslated)
			}
			out[c] = v
			res.CellsWritten++
		}
	}
}

// eliminate is step 3: scan every entity's Translated cells; a cell
// qualifies when its IC partner is set (not "" and not None) and its account
// is flagged IsIC. The negated value accumulates at the first common
// ancestor of (entity, partner) with Origin=Elim (collisions sum); a missing
// common ancestor is recorded once per (entity, partner) and the cell is
// skipped. Elimination stages are cleared everywhere, then rebuilt on each
// receiving entity (unit ensured).
func eliminate(meta *model.Metadata, store *cube.Store, keys []cube.UnitKey, cubeName, scenario, month string, res *Result, touched map[cube.UnitKey]bool) {
	acctDim := meta.Account()
	entityDim := meta.Entity()

	pending := map[string]cube.CellMap{}
	seenPair := map[string]bool{}
	for _, k := range keys {
		u := store.Unit(k)
		if u == nil {
			continue
		}
		tr := u.Stages[model.StageTranslated]
		for _, c := range sortedCoords(tr) {
			if c.IC == "" || c.IC == model.NoneMember {
				continue
			}
			am := acctDim.Get(c.Account)
			if am == nil || !am.IsIC {
				continue
			}
			fca := entityDim.FCA(k.Entity, c.IC)
			if fca == "" {
				pk := k.Entity + "|" + c.IC
				if !seenPair[pk] {
					seenPair[pk] = true
					res.Issues = append(res.Issues,
						fmt.Sprintf("no common ancestor for entity %q and IC partner %q", k.Entity, c.IC))
				}
				continue
			}
			ec := c
			ec.Origin = model.OriginElim
			pm := pending[fca]
			if pm == nil {
				pm = cube.CellMap{}
				pending[fca] = pm
			}
			pm[ec] -= tr[c]
		}
	}

	for _, k := range keys {
		if u := store.Unit(k); u != nil {
			u.ClearStage(model.StageElimination)
		}
	}

	fcas := make([]string, 0, len(pending))
	for f := range pending {
		fcas = append(fcas, f)
	}
	sort.Strings(fcas)
	for _, fca := range fcas {
		k := cube.UnitKey{Cube: cubeName, Entity: fca, Scenario: scenario, Time: month}
		u := store.Ensure(k)
		u.ClearStage(model.StageElimination)
		touched[k] = true
		var out cube.CellMap
		for _, c := range sortedCoords(pending[fca]) {
			v := pending[fca][c]
			if !writableValue(v, "eliminate", k, res) || math.Abs(v) < eps {
				continue
			}
			if out == nil {
				out = u.Stage(model.StageElimination)
			}
			out[c] = v
			res.CellsWritten++
		}
	}
}

// consolidate is step 4: post-order over every Entity root,
// Consolidated[E] = Translated[E] + Elimination[E]
// + Σ over children c of Consolidated[c] × (c.OwnershipPct/100),
// summed coord-by-coord with |v| < eps dropped. Stale Consolidated stages
// are cleared first; units are ensured only for entities that receive cells.
func consolidate(meta *model.Metadata, store *cube.Store, cubeName, scenario, month string, res *Result, touched map[cube.UnitKey]bool) {
	entityDim := meta.Entity()

	for _, k := range store.Keys(cube.UnitKey{Cube: cubeName, Scenario: scenario, Time: month}) {
		if u := store.Unit(k); u != nil {
			u.ClearStage(model.StageConsolidated)
		}
	}

	var walk func(entity string) cube.CellMap
	walk = func(entity string) cube.CellMap {
		combined := cube.CellMap{}
		if m := entityDim.Get(entity); m != nil {
			for _, child := range m.Children {
				cm := entityDim.Get(child)
				if cm == nil {
					continue
				}
				contrib := walk(child)
				pct := cm.OwnershipPct / 100
				for c, v := range contrib {
					combined[c] += v * pct
				}
			}
		}
		k := cube.UnitKey{Cube: cubeName, Entity: entity, Scenario: scenario, Time: month}
		if u := store.Unit(k); u != nil {
			for c, v := range u.Stages[model.StageTranslated] {
				combined[c] += v
			}
			for c, v := range u.Stages[model.StageElimination] {
				combined[c] += v
			}
		}
		for c, v := range combined {
			if !writableValue(v, "consolidate", k, res) || math.Abs(v) < eps {
				delete(combined, c)
			}
		}
		if len(combined) > 0 {
			u := store.Ensure(k)
			touched[k] = true
			out := u.Stage(model.StageConsolidated)
			for c, v := range combined {
				out[c] = v
			}
			res.CellsWritten += len(combined)
		}
		return combined
	}
	for _, root := range entityDim.Roots {
		walk(root)
	}
}

// writableValue reports whether a computed value may be stored. Non-finite
// values (arithmetic overflow of huge inputs) are skipped with an Issue
// recorded once per (step, unit): a single NaN/±Inf cell would make every
// JSON snapshot save fail.
func writableValue(v float64, step string, k cube.UnitKey, res *Result) bool {
	if !math.IsNaN(v) && !math.IsInf(v, 0) {
		return true
	}
	msg := fmt.Sprintf("%s produced a non-finite value for unit %s; cell skipped", step, k.Key())
	for _, is := range res.Issues {
		if is == msg {
			return false
		}
	}
	res.Issues = append(res.Issues, msg)
	return false
}

// restOf collapses a stored coordinate into its rest-tuple: the CellCoord
// minus Account, with Origin collapsed to "" (refs sum across origins).
func restOf(c cube.CellCoord) cube.CellCoord {
	c.Account, c.Origin = "", ""
	return c
}

// groupByTuple indexes a unit's Input cells by rest-tuple, summing values
// across origins per account.
func groupByTuple(input cube.CellMap) map[cube.CellCoord]map[string]float64 {
	out := map[cube.CellCoord]map[string]float64{}
	for c, v := range input {
		rt := restOf(c)
		m := out[rt]
		if m == nil {
			m = map[string]float64{}
			out[rt] = m
		}
		m[c.Account] += v
	}
	return out
}

// tupleUniverse returns, in deterministic order, every rest-tuple where at
// least one referenced account subtree has stored data.
func tupleUniverse(byTuple map[cube.CellCoord]map[string]float64, refs []string, weightOf func(string) map[string]float64) []cube.CellCoord {
	var out []cube.CellCoord
	for rt, accounts := range byTuple {
		hit := false
		for _, ref := range refs {
			w := weightOf(ref)
			for a := range accounts {
				if _, ok := w[a]; ok {
					hit = true
					break
				}
			}
			if hit {
				break
			}
		}
		if hit {
			out = append(out, rt)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// rollup sums the per-account values of one tuple weighted by the account
// subtree weight map.
func rollup(accounts map[string]float64, weights map[string]float64) float64 {
	var total float64
	for a, v := range accounts {
		if w, ok := weights[a]; ok {
			total += v * w
		}
	}
	return total
}

// subtreeWeights maps account and each of its descendants to the cumulative
// product of edge weights along the path up into account (account itself =
// 1), so cells stored AT non-leaf accounts contribute too. Unknown accounts
// still match cells stored at exactly that name.
func subtreeWeights(d *model.Dimension, account string) map[string]float64 {
	w := map[string]float64{account: 1}
	if d == nil {
		return w
	}
	var walk func(name string, acc float64)
	walk = func(name string, acc float64) {
		m := d.Get(name)
		if m == nil {
			return
		}
		for _, child := range m.Children {
			cm := d.Get(child)
			if cm == nil {
				continue
			}
			cw := acc * cm.Weight
			w[child] = cw
			walk(child, cw)
		}
	}
	walk(account, 1)
	return w
}

// sortedCoords returns the coordinates of a cell map ordered by Key().
func sortedCoords(m cube.CellMap) []cube.CellCoord {
	out := make([]cube.CellCoord, 0, len(m))
	for c := range m {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}
