// Query engine for the cube (SPEC §4): single-cell resolution (GetCell) and
// grid rendering (Query). The storage types live in types.go (hand-authored).
package cube

import (
	"fmt"

	"onetrickle/internal/model"
)

// maxDynDepth bounds nested dynamic-calc evaluation (cycle guard).
const maxDynDepth = 16

// Expand modes for AxisSpec.Expand.
const (
	ExpandMember   = "member"
	ExpandChildren = "children"
	ExpandLeaves   = "leaves"
	ExpandTree     = "tree"
)

// POV is a full point of view naming one queryable cell. Empty fields take
// the SPEC §4 defaults: View=Periodic, Stage=Consolidated, Flow/UD*=total of
// the dimension (sum over roots), Origin=""=sum over all origins present,
// IC=""=sum over all partners (IC="None" matches only the None partner).
type POV struct {
	Cube     string `json:"cube,omitempty"`
	Entity   string `json:"entity,omitempty"`
	Scenario string `json:"scenario,omitempty"`
	Time     string `json:"time,omitempty"`
	View     string `json:"view,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Account  string `json:"account,omitempty"`
	Flow     string `json:"flow,omitempty"`
	Origin   string `json:"origin,omitempty"`
	IC       string `json:"ic,omitempty"`
	UD1      string `json:"ud1,omitempty"`
	UD2      string `json:"ud2,omitempty"`
	UD3      string `json:"ud3,omitempty"`
	UD4      string `json:"ud4,omitempty"`
}

// DynEval evaluates a dynamic-calc account formula at a POV. getRef resolves
// an A# account reference at the same POV with only the account swapped.
// Provided by the calc package at wiring time; nil disables dynamic calcs.
type DynEval func(meta *model.Metadata, pov POV, formula string, getRef func(account string) float64) (float64, error)

// Engine answers dimensional queries against a Store.
type Engine struct {
	Store   *Store
	DynEval DynEval
}

// NewEngine returns an engine over s with no dynamic-calc evaluator.
func NewEngine(s *Store) *Engine { return &Engine{Store: s} }

// evalCtx threads dynamic-calc evaluation state through getCell recursion:
// the depth guard, the set of (account, POV) cells currently being evaluated
// (cycle detection), the cells on a detected cycle, and the issues recorded
// per SPEC §4 (deduplicated).
type evalCtx struct {
	depth    int
	inFlight map[POV]bool
	cycled   map[POV]bool
	issues   []string
	seen     map[string]bool
}

func newEvalCtx() *evalCtx {
	return &evalCtx{inFlight: map[POV]bool{}, cycled: map[POV]bool{}, seen: map[string]bool{}}
}

// record appends a (deduplicated) issue message.
func (c *evalCtx) record(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if c.seen[msg] {
		return
	}
	c.seen[msg] = true
	c.issues = append(c.issues, msg)
}

// GetCell resolves one cell value per SPEC §4. Account and Time are
// required; a missing coordinate, member or unit yields 0 (queries never
// fail on absent data). Dynamic-calc problems are dropped; use
// GetCellIssues to observe them.
func (e *Engine) GetCell(meta *model.Metadata, pov POV) float64 {
	v, _ := e.GetCellIssues(meta, pov)
	return v
}

// GetCellIssues is GetCell plus the dynamic-calc issues recorded during
// resolution (formula cycles, depth-limit hits, evaluation errors). The
// issue slice is nil when nothing went wrong.
func (e *Engine) GetCellIssues(meta *model.Metadata, pov POV) (float64, []string) {
	ctx := newEvalCtx()
	v := e.getCell(meta, pov, ctx)
	return v, ctx.issues
}

// getCell is GetCell with the dynamic-calc evaluation context carried
// through reference resolution.
func (e *Engine) getCell(meta *model.Metadata, pov POV, ctx *evalCtx) float64 {
	if pov.Account == "" || pov.Time == "" {
		return 0
	}
	if pov.View == "" {
		pov.View = model.ViewPeriodic
	}
	if pov.Stage == "" {
		pov.Stage = string(model.StageConsolidated)
	}

	// 1. Dynamic-calc account: evaluate the formula post-aggregation. Refs
	// resolve via getCell with only the account swapped, so every referenced
	// value is itself fully aggregated at this POV. A re-entered (account,
	// POV) is a formula cycle: the whole cell yields 0 and an issue is
	// recorded (SPEC §4); the depth guard backstops long acyclic chains.
	if am := meta.Account().Get(pov.Account); am != nil && am.DynamicCalc && am.Formula != "" {
		if e.DynEval == nil {
			return 0
		}
		if ctx.inFlight[pov] {
			ctx.cycled[pov] = true
			ctx.record("dynamic formula cycle at account %q", pov.Account)
			return 0
		}
		if ctx.depth >= maxDynDepth {
			ctx.record("dynamic formula depth limit (%d) reached at account %q", maxDynDepth, pov.Account)
			return 0
		}
		ctx.inFlight[pov] = true
		getRef := func(account string) float64 {
			ref := pov
			ref.Account = account
			ctx.depth++
			v := e.getCell(meta, ref, ctx)
			ctx.depth--
			return v
		}
		v, err := e.DynEval(meta, pov, am.Formula, getRef)
		delete(ctx.inFlight, pov)
		if err != nil {
			ctx.record("dynamic formula on account %q: %v", pov.Account, err)
			return 0
		}
		if ctx.cycled[pov] {
			return 0 // this cell sits on a formula cycle: SPEC §4 cycle → 0
		}
		return v
	}

	// 2. Time: resolve the POV time member to the list of months to read.
	// Balance accounts (time agg = Last) read only the final month of the
	// period under either view; Sum accounts read all months of the period
	// (Periodic) or Jan..final-month (YTD).
	months := model.MonthsUnder(meta.Dim(model.DimTime), pov.Time)
	if len(months) == 0 {
		return 0
	}
	final := months[len(months)-1]
	switch {
	case meta.AccountType(pov.Account).IsBalance():
		months = []string{final}
	case pov.View == model.ViewYTD:
		months = model.YTDMonths(final)
	}

	if e.Store == nil {
		return 0
	}

	// 3+4. Entity/stage selection and weighted rollup over each month's unit.
	group := ""
	if c := meta.Cubes[pov.Cube]; c != nil {
		group = c.GroupCurrency
	}
	entityCur := group
	if em := meta.Entity().Get(pov.Entity); em != nil && em.Currency != "" {
		entityCur = em.Currency
	}

	m := newMatcher(meta, pov)
	var total float64
	for _, month := range months {
		u := e.Store.Unit(UnitKey{Cube: pov.Cube, Entity: pov.Entity, Scenario: pov.Scenario, Time: month})
		total += m.sum(stageCells(u, model.ConsStage(pov.Stage), entityCur == group))
	}
	return total
}

// stageCells picks the cell map a stage reads from. Stage=Consolidated with
// no materialized Consolidated map falls back to Translated; failing that,
// to Input when the entity's currency equals the cube group currency
// (localIsGroup); else nothing. An unknown stage reads nothing.
func stageCells(u *Unit, stage model.ConsStage, localIsGroup bool) CellMap {
	if u == nil {
		return nil
	}
	switch stage {
	case model.StageLocal:
		return u.Input
	case model.StageTranslated, model.StageElimination:
		return u.Stages[stage]
	case model.StageConsolidated:
		if cm, ok := u.Stages[model.StageConsolidated]; ok && cm != nil {
			return cm
		}
		if cm, ok := u.Stages[model.StageTranslated]; ok && cm != nil {
			return cm
		}
		if localIsGroup {
			return u.Input
		}
	}
	return nil
}

// matcher tests a stored cell against a POV and yields its rollup weight.
// The per-dim maps hold the target member and every descendant, mapped to
// the product of edge weights along the path up to the target.
type matcher struct {
	account map[string]float64
	flow    map[string]float64
	ud1     map[string]float64
	ud2     map[string]float64
	ud3     map[string]float64
	ud4     map[string]float64
	origin  string // "" = any origin
	ic      string // "" = any IC partner
}

func newMatcher(meta *model.Metadata, pov POV) *matcher {
	return &matcher{
		account: memberWeights(meta.Dim(model.DimAccount), pov.Account),
		flow:    totalOrMemberWeights(meta.Dim(model.DimFlow), pov.Flow),
		ud1:     totalOrMemberWeights(meta.Dim(model.DimUD1), pov.UD1),
		ud2:     totalOrMemberWeights(meta.Dim(model.DimUD2), pov.UD2),
		ud3:     totalOrMemberWeights(meta.Dim(model.DimUD3), pov.UD3),
		ud4:     totalOrMemberWeights(meta.Dim(model.DimUD4), pov.UD4),
		origin:  pov.Origin,
		ic:      pov.IC,
	}
}

// sum folds every matching cell of one (unit, stage) cell map into a total,
// weighting each cell by the product of its per-dimension path weights.
func (m *matcher) sum(cells CellMap) float64 {
	var total float64
	for c, v := range cells {
		if m.origin != "" && c.Origin != m.origin {
			continue
		}
		if m.ic != "" && c.IC != m.ic {
			continue
		}
		wa, ok := m.account[c.Account]
		if !ok {
			continue
		}
		wf, ok := m.flow[c.Flow]
		if !ok {
			continue
		}
		w1, ok := m.ud1[c.UD1]
		if !ok {
			continue
		}
		w2, ok := m.ud2[c.UD2]
		if !ok {
			continue
		}
		w3, ok := m.ud3[c.UD3]
		if !ok {
			continue
		}
		w4, ok := m.ud4[c.UD4]
		if !ok {
			continue
		}
		total += v * wa * wf * w1 * w2 * w3 * w4
	}
	return total
}

// memberWeights maps target and each of its descendants to the cumulative
// edge weight along the path up into target (target itself = 1). Unknown
// targets still match cells stored at exactly that name.
func memberWeights(d *model.Dimension, target string) map[string]float64 {
	w := map[string]float64{target: 1}
	if d != nil {
		descendWeights(d, target, 1, w)
	}
	return w
}

// totalOrMemberWeights returns memberWeights for a named member, or — when
// member is empty — the dimension total: the union of every root's subtree,
// each root entering with weight 1.
func totalOrMemberWeights(d *model.Dimension, member string) map[string]float64 {
	if member != "" {
		return memberWeights(d, member)
	}
	w := map[string]float64{}
	if d != nil {
		for _, r := range d.Roots {
			w[r] = 1
			descendWeights(d, r, 1, w)
		}
	}
	return w
}

func descendWeights(d *model.Dimension, name string, acc float64, w map[string]float64) {
	m := d.Get(name)
	if m == nil {
		return
	}
	for _, c := range m.Children {
		cm := d.Get(c)
		if cm == nil {
			continue
		}
		cw := acc * cm.Weight
		w[c] = cw
		descendWeights(d, c, cw, w)
	}
}

// HeaderCell is one rendered row/column header. Depth is the distance from
// the AxisSpec member (for indentation in tree expansions).
type HeaderCell struct {
	Name   string `json:"name"`
	Depth  int    `json:"depth"`
	IsLeaf bool   `json:"isLeaf"`
}

// AxisSpec names one dimension slice of a query axis. Expand is one of
// member (default), children, leaves, tree.
type AxisSpec struct {
	Dim    string `json:"dim"`
	Member string `json:"member"`
	Expand string `json:"expand"`
}

// QueryRequest renders a grid: each axis is the concatenation of its
// expanded specs; every other coordinate comes from POV.
type QueryRequest struct {
	Cube string     `json:"cube"`
	POV  POV        `json:"pov"`
	Rows []AxisSpec `json:"rows"`
	Cols []AxisSpec `json:"cols"`
}

// QueryResult is the rendered grid; Cells is indexed [row][col]. Issues
// carries dynamic-calc problems recorded during resolution (formula cycles,
// depth-limit hits, evaluation errors), deduplicated; never nil so it
// serializes as [].
type QueryResult struct {
	RowHeaders []HeaderCell `json:"rowHeaders"`
	ColHeaders []HeaderCell `json:"colHeaders"`
	Cells      [][]float64  `json:"cells"`
	Issues     []string     `json:"issues"`
}

// axisCell pairs a rendered header with the dim it overlays onto the POV.
type axisCell struct {
	header HeaderCell
	dim    string
}

// Query expands the row/col axis specs, overlays each (row, col) member pair
// onto the request POV, and resolves every cell via GetCell.
func (e *Engine) Query(meta *model.Metadata, req QueryRequest) (*QueryResult, error) {
	base := req.POV
	if req.Cube != "" {
		base.Cube = req.Cube
	}
	if base.Cube != "" {
		if _, err := meta.CubeOf(base.Cube); err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
	}
	rows, err := expandAxes(meta, req.Rows)
	if err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	cols, err := expandAxes(meta, req.Cols)
	if err != nil {
		return nil, fmt.Errorf("cols: %w", err)
	}
	res := &QueryResult{
		RowHeaders: headersOf(rows),
		ColHeaders: headersOf(cols),
		Cells:      make([][]float64, len(rows)),
		Issues:     []string{},
	}
	ctx := newEvalCtx()
	for i, r := range rows {
		line := make([]float64, len(cols))
		for j, c := range cols {
			pov := base
			applyAxis(&pov, r.dim, r.header.Name)
			applyAxis(&pov, c.dim, c.header.Name)
			line[j] = e.getCell(meta, pov, ctx)
		}
		res.Cells[i] = line
	}
	res.Issues = append(res.Issues, ctx.issues...)
	return res, nil
}

func headersOf(cells []axisCell) []HeaderCell {
	out := make([]HeaderCell, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.header)
	}
	return out
}

// expandAxes concatenates the expansions of every spec on one axis.
func expandAxes(meta *model.Metadata, specs []AxisSpec) ([]axisCell, error) {
	out := []axisCell{}
	for i, s := range specs {
		cells, err := expandSpec(meta, s)
		if err != nil {
			return nil, fmt.Errorf("spec %d (dim %q): %w", i, s.Dim, err)
		}
		out = append(out, cells...)
	}
	return out, nil
}

// expandSpec validates the spec's dim, member and expand mode and produces
// its header cells. View and Stage are flat pseudo-dimensions; IC is None
// (flat) or an Entity member expanded against the Entity hierarchy.
func expandSpec(meta *model.Metadata, s AxisSpec) ([]axisCell, error) {
	expand := s.Expand
	if expand == "" {
		expand = ExpandMember
	}
	switch expand {
	case ExpandMember, ExpandChildren, ExpandLeaves, ExpandTree:
	default:
		return nil, fmt.Errorf("unknown expand %q (want member, children, leaves or tree)", s.Expand)
	}
	switch s.Dim {
	case "View":
		if s.Member != model.ViewPeriodic && s.Member != model.ViewYTD {
			return nil, fmt.Errorf("member %q not found in dimension View", s.Member)
		}
		return flatExpand(s.Dim, s.Member, expand), nil
	case "Stage":
		switch model.ConsStage(s.Member) {
		case model.StageLocal, model.StageTranslated, model.StageElimination, model.StageConsolidated:
		default:
			return nil, fmt.Errorf("member %q not found in dimension Stage", s.Member)
		}
		return flatExpand(s.Dim, s.Member, expand), nil
	case "IC":
		if s.Member == model.NoneMember {
			return flatExpand(s.Dim, s.Member, expand), nil
		}
		d := meta.Entity()
		if d == nil || !d.Has(s.Member) {
			return nil, fmt.Errorf("member %q not found in dimension IC (want None or an Entity member)", s.Member)
		}
		return dimExpand(d, s.Dim, s.Member, expand), nil
	default:
		dt, ok := axisDimType(s.Dim)
		if !ok {
			return nil, fmt.Errorf("unknown axis dim %q", s.Dim)
		}
		d := meta.Dim(dt)
		if d == nil || !d.Has(s.Member) {
			return nil, fmt.Errorf("member %q not found in dimension %s", s.Member, dt)
		}
		return dimExpand(d, s.Dim, s.Member, expand), nil
	}
}

// flatExpand expands a member of a flat pseudo-dimension: member/leaves/tree
// all yield the member itself; children yields nothing.
func flatExpand(dim, member, expand string) []axisCell {
	if expand == ExpandChildren {
		return []axisCell{}
	}
	return []axisCell{{header: HeaderCell{Name: member, Depth: 0, IsLeaf: true}, dim: dim}}
}

// dimExpand expands a member within a real dimension hierarchy. Tree depth
// is the distance from the spec member; other modes render flat (depth 0).
func dimExpand(d *model.Dimension, dim, member, expand string) []axisCell {
	out := []axisCell{}
	add := func(name string, depth int) {
		out = append(out, axisCell{header: HeaderCell{Name: name, Depth: depth, IsLeaf: d.IsLeaf(name)}, dim: dim})
	}
	switch expand {
	case ExpandMember:
		add(member, 0)
	case ExpandChildren:
		for _, c := range d.Get(member).Children {
			add(c, 0)
		}
	case ExpandLeaves:
		for _, l := range d.Leaves(member) {
			add(l, 0)
		}
	case ExpandTree:
		var walk func(name string, depth int)
		walk = func(name string, depth int) {
			add(name, depth)
			for _, c := range d.Get(name).Children {
				walk(c, depth+1)
			}
		}
		walk(member, 0)
	}
	return out
}

// axisDimType maps an axis dim name onto its stored dimension type.
func axisDimType(dim string) (model.DimType, bool) {
	switch model.DimType(dim) {
	case model.DimEntity, model.DimAccount, model.DimScenario, model.DimTime,
		model.DimFlow, model.DimOrigin,
		model.DimUD1, model.DimUD2, model.DimUD3, model.DimUD4:
		return model.DimType(dim), true
	}
	return "", false
}

// applyAxis overlays one axis member onto the POV. Dims are validated during
// expansion, so unknown names are ignored here.
func applyAxis(p *POV, dim, member string) {
	switch dim {
	case string(model.DimEntity):
		p.Entity = member
	case string(model.DimAccount):
		p.Account = member
	case string(model.DimScenario):
		p.Scenario = member
	case string(model.DimTime):
		p.Time = member
	case string(model.DimFlow):
		p.Flow = member
	case string(model.DimOrigin):
		p.Origin = member
	case string(model.DimUD1):
		p.UD1 = member
	case string(model.DimUD2):
		p.UD2 = member
	case string(model.DimUD3):
		p.UD3 = member
	case string(model.DimUD4):
		p.UD4 = member
	case "View":
		p.View = member
	case "Stage":
		p.Stage = member
	case "IC":
		p.IC = member
	}
}
