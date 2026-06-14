package cube

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"onetrickle/internal/model"
)

const eps = 1e-9

func approxEq(a, b float64) bool { return math.Abs(a-b) <= eps }

// newTestMeta builds the shared metadata fixture:
//
//	Cube Main (USD)
//	Entity:  Global(USD) -> US(USD), DE(EUR), FR(EUR)
//	Account: NetIncome -> Sales(+1, IsIC), Expenses(-1) -> Salaries, Rent
//	         Cash(Asset); Margin/Loop/ErrCalc/DynEmpty (dynamic calcs)
//	Flow:    None -> Open
//	UD1:     None; Regions -> East(+1), West(-1)
//	Time:    2025; Scenarios Actual, Budget
func newTestMeta(t *testing.T) *model.Metadata {
	t.Helper()
	meta := model.NewMetadata()
	meta.Cubes["Main"] = &model.Cube{Name: "Main", GroupCurrency: "USD"}
	add := func(dt model.DimType, m *model.Member) {
		t.Helper()
		if err := meta.Dim(dt).AddMember(m); err != nil {
			t.Fatalf("add %s member %q: %v", dt, m.Name, err)
		}
	}
	add(model.DimEntity, &model.Member{Name: "Global", Currency: "USD"})
	add(model.DimEntity, &model.Member{Name: "US", Parent: "Global", Currency: "USD"})
	add(model.DimEntity, &model.Member{Name: "DE", Parent: "Global", Currency: "EUR"})
	add(model.DimEntity, &model.Member{Name: "FR", Parent: "Global", Currency: "EUR"})

	add(model.DimAccount, &model.Member{Name: "NetIncome", AccountType: model.AccountRevenue})
	add(model.DimAccount, &model.Member{Name: "Sales", Parent: "NetIncome", AccountType: model.AccountRevenue, IsIC: true})
	add(model.DimAccount, &model.Member{Name: "Expenses", Parent: "NetIncome", Weight: -1, AccountType: model.AccountExpense})
	add(model.DimAccount, &model.Member{Name: "Salaries", Parent: "Expenses", AccountType: model.AccountExpense})
	add(model.DimAccount, &model.Member{Name: "Rent", Parent: "Expenses", AccountType: model.AccountExpense})
	add(model.DimAccount, &model.Member{Name: "Cash", AccountType: model.AccountAsset})
	add(model.DimAccount, &model.Member{Name: "Margin", AccountType: model.AccountNonFinancial, DynamicCalc: true, Formula: "Sales-Expenses"})
	add(model.DimAccount, &model.Member{Name: "Loop", AccountType: model.AccountNonFinancial, DynamicCalc: true, Formula: "Loop"})
	add(model.DimAccount, &model.Member{Name: "ErrCalc", AccountType: model.AccountNonFinancial, DynamicCalc: true, Formula: "ERR"})
	add(model.DimAccount, &model.Member{Name: "DynEmpty", AccountType: model.AccountNonFinancial, DynamicCalc: true})

	add(model.DimScenario, &model.Member{Name: "Actual"})
	add(model.DimScenario, &model.Member{Name: "Budget"})

	add(model.DimFlow, &model.Member{Name: "Open", Parent: model.NoneMember})
	add(model.DimUD1, &model.Member{Name: "Regions"})
	add(model.DimUD1, &model.Member{Name: "East", Parent: "Regions"})
	add(model.DimUD1, &model.Member{Name: "West", Parent: "Regions", Weight: -1})

	model.AddTimeYear(meta.Dim(model.DimTime), 2025)
	return meta
}

func coord(account, flow, origin, ic, ud1 string) CellCoord {
	return CellCoord{Account: account, Flow: flow, Origin: origin, IC: ic, UD1: ud1}.Normalize()
}

func unitKey(entity, scenario, month string) UnitKey {
	return UnitKey{Cube: "Main", Entity: entity, Scenario: scenario, Time: month}
}

// newTestStore loads the shared data fixture (values in test comments below).
func newTestStore() *Store {
	s := NewStore()
	put := func(k UnitKey, stage model.ConsStage, c CellCoord, v float64) {
		s.Ensure(k).Stage(stage)[c] = v
	}

	// US Actual: Input only (no materialized stages).
	usM1 := unitKey("US", "Actual", "2025M1")
	put(usM1, model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 1000)
	put(usM1, model.StageLocal, coord("Sales", "", model.OriginImport, "DE", ""), 200)
	put(usM1, model.StageLocal, coord("Sales", "", model.OriginAdj, "", ""), 50)
	put(usM1, model.StageLocal, coord("Salaries", "", model.OriginImport, "", ""), 300)
	put(usM1, model.StageLocal, coord("Rent", "", model.OriginImport, "", ""), 100)
	put(usM1, model.StageLocal, coord("Cash", "", model.OriginImport, "", ""), 500)
	// Stored-calc result at a non-leaf account.
	put(usM1, model.StageLocal, coord("NetIncome", "", model.OriginCalc, "", ""), 850)

	usM2 := unitKey("US", "Actual", "2025M2")
	put(usM2, model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 1100)
	put(usM2, model.StageLocal, coord("Cash", "", model.OriginImport, "", ""), 600)

	usM3 := unitKey("US", "Actual", "2025M3")
	put(usM3, model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 1200)
	put(usM3, model.StageLocal, coord("Cash", "", model.OriginImport, "", ""), 700)

	// DE Actual M1: Input (EUR) + Translated, no Consolidated map.
	deM1 := unitKey("DE", "Actual", "2025M1")
	put(deM1, model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 400)
	put(deM1, model.StageLocal, coord("Cash", "", model.OriginImport, "", ""), 300)
	put(deM1, model.StageTranslated, coord("Sales", "", model.OriginImport, "", ""), 440)
	put(deM1, model.StageTranslated, coord("Cash", "", model.OriginImport, "", ""), 324)

	// FR Actual M1: Input only, EUR entity (no Consolidated fallback to Input).
	put(unitKey("FR", "Actual", "2025M1"), model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 100)

	// Global Actual M1: materialized Elimination + Consolidated, empty Input.
	glM1 := unitKey("Global", "Actual", "2025M1")
	put(glM1, model.StageElimination, coord("Sales", "", model.OriginElim, "DE", ""), -200)
	put(glM1, model.StageConsolidated, coord("Sales", "", model.OriginImport, "", ""), 1400)
	put(glM1, model.StageConsolidated, coord("Sales", "", model.OriginImport, "DE", ""), 200)
	put(glM1, model.StageConsolidated, coord("Sales", "", model.OriginElim, "DE", ""), -200)
	put(glM1, model.StageConsolidated, coord("Salaries", "", model.OriginImport, "", ""), 300)

	// US Budget M1: Flow / UD1 rollup fixture.
	usB := unitKey("US", "Budget", "2025M1")
	put(usB, model.StageLocal, coord("Sales", "", model.OriginImport, "", "East"), 10)
	put(usB, model.StageLocal, coord("Sales", "", model.OriginImport, "", "West"), 20)
	put(usB, model.StageLocal, coord("Sales", "", model.OriginImport, "", ""), 5)
	put(usB, model.StageLocal, coord("Sales", "Open", model.OriginImport, "", ""), 7)
	return s
}

func newTestEngine() *Engine { return NewEngine(newTestStore()) }

// stubDynEval interprets the fixture formulas without the calc package.
func stubDynEval(calls *int) DynEval {
	return func(meta *model.Metadata, pov POV, formula string, getRef func(string) float64) (float64, error) {
		if calls != nil {
			*calls++
		}
		switch formula {
		case "Sales-Expenses":
			return getRef("Sales") - getRef("Expenses"), nil
		case "Loop":
			return getRef("Loop") + 1, nil
		case "ERR":
			return 0, fmt.Errorf("eval failed")
		}
		return 0, fmt.Errorf("unknown formula %q", formula)
	}
}

func TestGetCellAccountRollupOriginIC(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	base := POV{Cube: "Main", Entity: "US", Scenario: "Actual", Time: "2025M1", Stage: string(model.StageLocal)}
	pov := func(mut func(*POV)) POV { p := base; mut(&p); return p }

	tests := []struct {
		name string
		pov  POV
		want float64
	}{
		{"leaf account all origins", pov(func(p *POV) { p.Account = "Salaries" }), 300},
		{"account with IC and Adj cells", pov(func(p *POV) { p.Account = "Sales" }), 1250},
		{"origin filter Import", pov(func(p *POV) { p.Account = "Sales"; p.Origin = model.OriginImport }), 1200},
		{"origin filter Adj", pov(func(p *POV) { p.Account = "Sales"; p.Origin = model.OriginAdj }), 50},
		{"origin filter Calc on parent", pov(func(p *POV) { p.Account = "NetIncome"; p.Origin = model.OriginCalc }), 850},
		{"IC partner filter", pov(func(p *POV) { p.Account = "Sales"; p.IC = "DE" }), 200},
		{"IC None matches only None", pov(func(p *POV) { p.Account = "Sales"; p.IC = model.NoneMember }), 1050},
		{"parent rollup weight +1", pov(func(p *POV) { p.Account = "Expenses" }), 400},
		{"weighted rollup with direct non-leaf cell", pov(func(p *POV) { p.Account = "NetIncome" }), 1700}, // 1250 - 400 + 850
		{"unknown account", pov(func(p *POV) { p.Account = "Nope" }), 0},
		{"empty account required", pov(func(p *POV) { p.Account = "" }), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.GetCell(meta, tc.pov); !approxEq(got, tc.want) {
				t.Errorf("GetCell(%+v) = %v, want %v", tc.pov, got, tc.want)
			}
		})
	}
}

func TestGetCellTimeAggregation(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	base := POV{Cube: "Main", Entity: "US", Scenario: "Actual", Stage: string(model.StageLocal)}
	pov := func(account, tm, view string) POV {
		p := base
		p.Account, p.Time, p.View = account, tm, view
		return p
	}

	tests := []struct {
		name string
		pov  POV
		want float64
	}{
		{"sum account single month", pov("Sales", "2025M1", ""), 1250},
		{"sum account quarter", pov("Sales", "2025Q1", ""), 3550}, // 1250+1100+1200
		{"sum account year", pov("Sales", "2025", ""), 3550},
		{"balance account month", pov("Cash", "2025M2", ""), 600},
		{"balance account quarter = final month", pov("Cash", "2025Q1", ""), 700},
		{"balance quarter with no data = 0", pov("Cash", "2025Q2", ""), 0},
		{"balance year = M12 even if empty", pov("Cash", "2025", ""), 0},
		{"YTD sum at month", pov("Sales", "2025M2", model.ViewYTD), 2350}, // 1250+1100
		{"YTD balance at month", pov("Cash", "2025M2", model.ViewYTD), 600},
		{"YTD sum at quarter = YTD at final month", pov("Sales", "2025Q1", model.ViewYTD), 3550},
		{"YTD balance at quarter", pov("Cash", "2025Q1", model.ViewYTD), 700},
		{"YTD sum at year", pov("Sales", "2025", model.ViewYTD), 3550},
		{"YTD balance at year", pov("Cash", "2025", model.ViewYTD), 0},
		{"YTD parent account", pov("NetIncome", "2025M2", model.ViewYTD), 2800}, // 1700+1100
		{"empty time required", pov("Sales", "", ""), 0},
		{"unknown time member", pov("Sales", "2030M1", ""), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.GetCell(meta, tc.pov); !approxEq(got, tc.want) {
				t.Errorf("GetCell(%+v) = %v, want %v", tc.pov, got, tc.want)
			}
		})
	}
}

func TestGetCellStages(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	pov := func(entity, account, stage string, mut ...func(*POV)) POV {
		p := POV{Cube: "Main", Entity: entity, Scenario: "Actual", Time: "2025M1", Account: account, Stage: stage}
		for _, m := range mut {
			m(&p)
		}
		return p
	}
	local := string(model.StageLocal)
	cons := string(model.StageConsolidated)

	tests := []struct {
		name string
		pov  POV
		want float64
	}{
		{"local reads input", pov("DE", "Sales", local), 400},
		{"translated reads stage map", pov("DE", "Sales", string(model.StageTranslated)), 440},
		{"consolidated falls back to translated", pov("DE", "Sales", cons), 440},
		{"consolidated fallback balance cell", pov("DE", "Cash", cons), 324},
		{"default stage is consolidated", pov("DE", "Sales", ""), 440},
		{"consolidated falls back to input when currency = group", pov("US", "Sales", cons), 1250},
		{"no fallback to input for non-group currency", pov("FR", "Sales", cons), 0},
		{"local on same entity still reads input", pov("FR", "Sales", local), 100},
		{"parent entity reads own unit, not children (local)", pov("Global", "Sales", local), 0},
		{"parent consolidated map incl elim origin", pov("Global", "Sales", cons), 1400}, // 1400+200-200
		{"parent consolidated origin Elim", pov("Global", "Sales", cons, func(p *POV) { p.Origin = model.OriginElim }), -200},
		{"parent consolidated origin Import", pov("Global", "Sales", cons, func(p *POV) { p.Origin = model.OriginImport }), 1600},
		{"parent consolidated IC nets to zero", pov("Global", "Sales", cons, func(p *POV) { p.IC = "DE" }), 0},
		{"parent consolidated IC None", pov("Global", "Sales", cons, func(p *POV) { p.IC = model.NoneMember }), 1400},
		{"elimination stage", pov("Global", "Sales", string(model.StageElimination)), -200},
		{"elimination stage absent on entity", pov("DE", "Sales", string(model.StageElimination)), 0},
		{"parent consolidated weighted rollup", pov("Global", "NetIncome", cons), 1100}, // 1400 - 300
		{"missing unit", pov("DE", "Sales", local, func(p *POV) { p.Scenario = "Budget" }), 0},
		{"unknown stage", pov("US", "Sales", "Bogus"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.GetCell(meta, tc.pov); !approxEq(got, tc.want) {
				t.Errorf("GetCell(%+v) = %v, want %v", tc.pov, got, tc.want)
			}
		})
	}
}

func TestGetCellFlowAndUDRollup(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	base := POV{Cube: "Main", Entity: "US", Scenario: "Budget", Time: "2025M1", Account: "Sales", Stage: string(model.StageLocal)}
	pov := func(mut func(*POV)) POV { p := base; mut(&p); return p }

	tests := []struct {
		name string
		pov  POV
		want float64
	}{
		{"flow and UD1 empty = dim totals", base, 2}, // 10 - 20 + 5 + 7
		{"UD1 None member only", pov(func(p *POV) { p.UD1 = model.NoneMember }), 12},
		{"UD1 leaf", pov(func(p *POV) { p.UD1 = "East" }), 10},
		{"UD1 leaf queried directly ignores own weight", pov(func(p *POV) { p.UD1 = "West" }), 20},
		{"UD1 weighted parent rollup", pov(func(p *POV) { p.UD1 = "Regions" }), -10}, // 10 - 20
		{"flow root rolls up children", pov(func(p *POV) { p.Flow = model.NoneMember }), 2},
		{"flow leaf", pov(func(p *POV) { p.Flow = "Open" }), 7},
		{"flow and UD combined", pov(func(p *POV) { p.Flow = "Open"; p.UD1 = "East" }), 0},
		{"unused UD dims default to total", pov(func(p *POV) { p.UD2 = model.NoneMember }), 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.GetCell(meta, tc.pov); !approxEq(got, tc.want) {
				t.Errorf("GetCell(%+v) = %v, want %v", tc.pov, got, tc.want)
			}
		})
	}
}

func TestGetCellDynamicCalc(t *testing.T) {
	meta := newTestMeta(t)
	pov := func(account, tm string) POV {
		return POV{Cube: "Main", Entity: "US", Scenario: "Actual", Time: tm, Account: account, Stage: string(model.StageLocal)}
	}

	t.Run("evaluates after aggregation", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		if got := e.GetCell(meta, pov("Margin", "2025M1")); !approxEq(got, 850) { // 1250 - 400
			t.Errorf("Margin M1 = %v, want 850", got)
		}
		if got := e.GetCell(meta, pov("Margin", "2025Q1")); !approxEq(got, 3150) { // 3550 - 400
			t.Errorf("Margin Q1 = %v, want 3150", got)
		}
	})

	t.Run("self-referential cycle yields 0 and records an issue", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		got, issues := e.GetCellIssues(meta, pov("Loop", "2025M1"))
		if !approxEq(got, 0) {
			t.Errorf("Loop = %v, want 0 (cycle)", got)
		}
		if len(issues) != 1 || !strings.Contains(issues[0], "cycle") || !strings.Contains(issues[0], "Loop") {
			t.Errorf("issues = %v, want one cycle issue naming Loop", issues)
		}
	})

	t.Run("acyclic chain past depth 16 records a depth issue", func(t *testing.T) {
		meta2 := newTestMeta(t)
		for i := 0; i <= 17; i++ {
			m := &model.Member{Name: fmt.Sprintf("C%02d", i), AccountType: model.AccountNonFinancial,
				DynamicCalc: true, Formula: "CHAIN"}
			if err := meta2.Dim(model.DimAccount).AddMember(m); err != nil {
				t.Fatalf("add chain account: %v", err)
			}
		}
		e := newTestEngine()
		e.DynEval = func(_ *model.Metadata, p POV, formula string, getRef func(string) float64) (float64, error) {
			var i int
			if _, err := fmt.Sscanf(p.Account, "C%d", &i); err != nil {
				return 0, err
			}
			return getRef(fmt.Sprintf("C%02d", i+1)) + 1, nil
		}
		p := pov("C00", "2025M1")
		got, issues := e.GetCellIssues(meta2, p)
		if !approxEq(got, 16) {
			t.Errorf("chain = %v, want 16 (+1 per allowed depth)", got)
		}
		if len(issues) != 1 || !strings.Contains(issues[0], "depth limit") {
			t.Errorf("issues = %v, want one depth-limit issue", issues)
		}
	})

	t.Run("eval error yields zero and records an issue", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		got, issues := e.GetCellIssues(meta, pov("ErrCalc", "2025M1"))
		if !approxEq(got, 0) {
			t.Errorf("ErrCalc = %v, want 0", got)
		}
		if len(issues) != 1 || !strings.Contains(issues[0], "ErrCalc") {
			t.Errorf("issues = %v, want one issue naming ErrCalc", issues)
		}
	})

	t.Run("GetCell drops issues but still yields 0 on cycle", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		if got := e.GetCell(meta, pov("Loop", "2025M1")); !approxEq(got, 0) {
			t.Errorf("Loop = %v, want 0", got)
		}
	})

	t.Run("query surfaces dynamic-calc issues", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		res, err := e.Query(meta, QueryRequest{
			Cube: "Main",
			POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M1", Stage: string(model.StageLocal)},
			Rows: []AxisSpec{{Dim: "Account", Member: "Loop"}, {Dim: "Account", Member: "Margin"}},
			Cols: []AxisSpec{{Dim: "Time", Member: "2025M1"}},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !approxEq(res.Cells[0][0], 0) || !approxEq(res.Cells[1][0], 850) {
			t.Errorf("cells = %v, want [[0] [850]]", res.Cells)
		}
		if len(res.Issues) != 1 || !strings.Contains(res.Issues[0], "cycle") {
			t.Errorf("query issues = %v, want one deduplicated cycle issue", res.Issues)
		}
	})

	t.Run("clean query has empty non-nil issues", func(t *testing.T) {
		e := newTestEngine()
		e.DynEval = stubDynEval(nil)
		res, err := e.Query(meta, QueryRequest{
			Cube: "Main",
			POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M1", Stage: string(model.StageLocal)},
			Rows: []AxisSpec{{Dim: "Account", Member: "Margin"}},
			Cols: []AxisSpec{{Dim: "Time", Member: "2025M1"}},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if res.Issues == nil || len(res.Issues) != 0 {
			t.Errorf("issues = %#v, want non-nil empty slice", res.Issues)
		}
	})

	t.Run("nil DynEval yields zero", func(t *testing.T) {
		e := newTestEngine()
		if got := e.GetCell(meta, pov("Margin", "2025M1")); !approxEq(got, 0) {
			t.Errorf("Margin with nil DynEval = %v, want 0", got)
		}
	})

	t.Run("dynamic flag without formula is a normal account", func(t *testing.T) {
		e := newTestEngine()
		calls := 0
		e.DynEval = stubDynEval(&calls)
		if got := e.GetCell(meta, pov("DynEmpty", "2025M1")); !approxEq(got, 0) {
			t.Errorf("DynEmpty = %v, want 0", got)
		}
		if calls != 0 {
			t.Errorf("DynEval called %d times for formula-less account, want 0", calls)
		}
	})
}

func TestGetCellDefaultsAndMissing(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()

	implicit := POV{Cube: "Main", Entity: "DE", Scenario: "Actual", Time: "2025Q1", Account: "Sales"}
	explicit := implicit
	explicit.View = model.ViewPeriodic
	explicit.Stage = string(model.StageConsolidated)
	if got, want := e.GetCell(meta, implicit), e.GetCell(meta, explicit); !approxEq(got, want) {
		t.Errorf("default View/Stage: got %v, explicit Periodic/Consolidated %v", got, want)
	}
	if got := e.GetCell(meta, implicit); !approxEq(got, 440) {
		t.Errorf("DE Q1 default stage = %v, want 440 (translated fallback, M1 only)", got)
	}

	if got := NewEngine(nil).GetCell(meta, explicit); !approxEq(got, 0) {
		t.Errorf("nil store = %v, want 0", got)
	}
}

func TestQueryTreeGrid(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Stage: string(model.StageLocal)},
		Rows: []AxisSpec{{Dim: "Account", Member: "NetIncome", Expand: ExpandTree}},
		Cols: []AxisSpec{{Dim: "Time", Member: "2025Q1", Expand: ExpandChildren}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	wantRows := []HeaderCell{
		{Name: "NetIncome", Depth: 0, IsLeaf: false},
		{Name: "Sales", Depth: 1, IsLeaf: true},
		{Name: "Expenses", Depth: 1, IsLeaf: false},
		{Name: "Salaries", Depth: 2, IsLeaf: true},
		{Name: "Rent", Depth: 2, IsLeaf: true},
	}
	if len(res.RowHeaders) != len(wantRows) {
		t.Fatalf("row headers = %+v, want %+v", res.RowHeaders, wantRows)
	}
	for i, w := range wantRows {
		if res.RowHeaders[i] != w {
			t.Errorf("row header %d = %+v, want %+v", i, res.RowHeaders[i], w)
		}
	}
	wantCols := []HeaderCell{
		{Name: "2025M1", Depth: 0, IsLeaf: true},
		{Name: "2025M2", Depth: 0, IsLeaf: true},
		{Name: "2025M3", Depth: 0, IsLeaf: true},
	}
	if len(res.ColHeaders) != len(wantCols) {
		t.Fatalf("col headers = %+v, want %+v", res.ColHeaders, wantCols)
	}
	for i, w := range wantCols {
		if res.ColHeaders[i] != w {
			t.Errorf("col header %d = %+v, want %+v", i, res.ColHeaders[i], w)
		}
	}
	wantCells := [][]float64{
		{1700, 1100, 1200},
		{1250, 1100, 1200},
		{400, 0, 0},
		{300, 0, 0},
		{100, 0, 0},
	}
	for i := range wantCells {
		for j := range wantCells[i] {
			if !approxEq(res.Cells[i][j], wantCells[i][j]) {
				t.Errorf("cell[%d][%d] = %v, want %v", i, j, res.Cells[i][j], wantCells[i][j])
			}
		}
	}
}

func TestQueryLeavesAndConcatenation(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Stage: string(model.StageLocal)},
		Rows: []AxisSpec{
			{Dim: "Account", Member: "Expenses", Expand: ExpandLeaves},
			{Dim: "Account", Member: "Cash"}, // empty Expand defaults to member
		},
		Cols: []AxisSpec{{Dim: "Time", Member: "2025M1", Expand: ExpandMember}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	wantNames := []string{"Salaries", "Rent", "Cash"}
	if len(res.RowHeaders) != len(wantNames) {
		t.Fatalf("row headers = %+v, want names %v", res.RowHeaders, wantNames)
	}
	for i, n := range wantNames {
		h := res.RowHeaders[i]
		if h.Name != n || h.Depth != 0 || !h.IsLeaf {
			t.Errorf("row header %d = %+v, want {%s 0 true}", i, h, n)
		}
	}
	wantVals := []float64{300, 100, 500}
	for i, w := range wantVals {
		if !approxEq(res.Cells[i][0], w) {
			t.Errorf("cell[%d][0] = %v, want %v", i, res.Cells[i][0], w)
		}
	}
}

// pathEq compares a header tuple against want as (dim,name,depth,isLeaf).
func pathEq(got []HeaderPart, want []HeaderPart) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestQueryNestedAxes(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()

	// Nested rows: Entity[US,DE] (outer) crossed with Account[Sales,Cash]
	// (inner), against a flat single-column Time axis. Cols stay flat, so
	// ColPaths must remain nil while RowPaths carries the 2-level tuple.
	t.Run("nested rows, flat cols", func(t *testing.T) {
		res, err := e.Query(meta, QueryRequest{
			Cube: "Main",
			POV:  POV{Scenario: "Actual", Stage: string(model.StageLocal)},
			RowNest: [][]AxisSpec{
				{{Dim: "Entity", Member: "US"}, {Dim: "Entity", Member: "DE"}},
				{{Dim: "Account", Member: "Sales"}, {Dim: "Account", Member: "Cash"}},
			},
			Cols: []AxisSpec{{Dim: "Time", Member: "2025M1"}},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		// Innermost headers are the Account level.
		wantHead := []string{"Sales", "Cash", "Sales", "Cash"}
		if len(res.RowHeaders) != len(wantHead) {
			t.Fatalf("row headers = %+v, want innermost %v", res.RowHeaders, wantHead)
		}
		for i, n := range wantHead {
			if res.RowHeaders[i].Name != n {
				t.Errorf("row header %d = %q, want %q", i, res.RowHeaders[i].Name, n)
			}
		}
		wantPaths := [][]HeaderPart{
			{{Dim: "Entity", Name: "US", IsLeaf: true}, {Dim: "Account", Name: "Sales", IsLeaf: true}},
			{{Dim: "Entity", Name: "US", IsLeaf: true}, {Dim: "Account", Name: "Cash", IsLeaf: true}},
			{{Dim: "Entity", Name: "DE", IsLeaf: true}, {Dim: "Account", Name: "Sales", IsLeaf: true}},
			{{Dim: "Entity", Name: "DE", IsLeaf: true}, {Dim: "Account", Name: "Cash", IsLeaf: true}},
		}
		if len(res.RowPaths) != len(wantPaths) {
			t.Fatalf("row paths = %+v, want %+v", res.RowPaths, wantPaths)
		}
		for i := range wantPaths {
			if !pathEq(res.RowPaths[i], wantPaths[i]) {
				t.Errorf("row path %d = %+v, want %+v", i, res.RowPaths[i], wantPaths[i])
			}
		}
		if res.ColPaths != nil {
			t.Errorf("flat cols should have nil ColPaths, got %+v", res.ColPaths)
		}
		wantCells := []float64{1250, 500, 400, 300} // US/Sales, US/Cash, DE/Sales, DE/Cash
		for i, w := range wantCells {
			if !approxEq(res.Cells[i][0], w) {
				t.Errorf("cell[%d][0] = %v, want %v", i, res.Cells[i][0], w)
			}
		}
	})

	// Nested rows AND nested cols: Time[M1,M2] crossed with Stage[Local].
	t.Run("nested rows and cols", func(t *testing.T) {
		res, err := e.Query(meta, QueryRequest{
			Cube: "Main",
			POV:  POV{Scenario: "Actual"},
			RowNest: [][]AxisSpec{
				{{Dim: "Entity", Member: "US"}, {Dim: "Entity", Member: "DE"}},
				{{Dim: "Account", Member: "Sales"}, {Dim: "Account", Member: "Cash"}},
			},
			ColNest: [][]AxisSpec{
				{{Dim: "Time", Member: "2025M1"}, {Dim: "Time", Member: "2025M2"}},
				{{Dim: "Stage", Member: string(model.StageLocal)}},
			},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		wantCol := [][]HeaderPart{
			{{Dim: "Time", Name: "2025M1", IsLeaf: true}, {Dim: "Stage", Name: "Local", IsLeaf: true}},
			{{Dim: "Time", Name: "2025M2", IsLeaf: true}, {Dim: "Stage", Name: "Local", IsLeaf: true}},
		}
		if len(res.ColPaths) != len(wantCol) {
			t.Fatalf("col paths = %+v, want %+v", res.ColPaths, wantCol)
		}
		for j := range wantCol {
			if !pathEq(res.ColPaths[j], wantCol[j]) {
				t.Errorf("col path %d = %+v, want %+v", j, res.ColPaths[j], wantCol[j])
			}
		}
		// Rows: US/Sales, US/Cash, DE/Sales, DE/Cash; Cols: M1 Local, M2 Local.
		wantCells := [][]float64{
			{1250, 1100}, // US Sales
			{500, 600},   // US Cash
			{400, 0},     // DE Sales (no DE M2 data)
			{300, 0},     // DE Cash
		}
		for i := range wantCells {
			for j := range wantCells[i] {
				if !approxEq(res.Cells[i][j], wantCells[i][j]) {
					t.Errorf("cell[%d][%d] = %v, want %v", i, j, res.Cells[i][j], wantCells[i][j])
				}
			}
		}
	})

	// A flat (single-level) axis must not emit paths — backward compatibility.
	t.Run("flat axis emits no paths", func(t *testing.T) {
		res, err := e.Query(meta, QueryRequest{
			Cube: "Main",
			POV:  POV{Entity: "US", Scenario: "Actual", Stage: string(model.StageLocal)},
			Rows: []AxisSpec{{Dim: "Account", Member: "NetIncome", Expand: ExpandTree}},
			Cols: []AxisSpec{{Dim: "Time", Member: "2025M1"}},
		})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if res.RowPaths != nil || res.ColPaths != nil {
			t.Errorf("flat axes should emit nil paths, got rows=%+v cols=%+v", res.RowPaths, res.ColPaths)
		}
	})
}

func TestQueryViewAndStageAxes(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()

	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M2", Stage: string(model.StageLocal)},
		Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}},
		Cols: []AxisSpec{{Dim: "View", Member: model.ViewPeriodic}, {Dim: "View", Member: model.ViewYTD}},
	})
	if err != nil {
		t.Fatalf("Query (view axis): %v", err)
	}
	if !approxEq(res.Cells[0][0], 1100) || !approxEq(res.Cells[0][1], 2350) {
		t.Errorf("view axis cells = %v, want [1100 2350]", res.Cells[0])
	}

	res, err = e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "DE", Scenario: "Actual", Time: "2025M1"},
		Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}},
		Cols: []AxisSpec{
			{Dim: "Stage", Member: string(model.StageLocal)},
			{Dim: "Stage", Member: string(model.StageTranslated)},
			{Dim: "Stage", Member: string(model.StageConsolidated)},
		},
	})
	if err != nil {
		t.Fatalf("Query (stage axis): %v", err)
	}
	want := []float64{400, 440, 440}
	for j, w := range want {
		if !approxEq(res.Cells[0][j], w) {
			t.Errorf("stage axis cell[0][%d] = %v, want %v", j, res.Cells[0][j], w)
		}
	}
}

func TestQueryTimeTree(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Stage: string(model.StageLocal)},
		Rows: []AxisSpec{{Dim: "Time", Member: "2025", Expand: ExpandTree}},
		Cols: []AxisSpec{{Dim: "Account", Member: "Cash"}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.RowHeaders) != 17 { // year + 4 quarters + 12 months
		t.Fatalf("time tree headers = %d, want 17", len(res.RowHeaders))
	}
	checks := []struct {
		idx  int
		want HeaderCell
	}{
		{0, HeaderCell{Name: "2025", Depth: 0, IsLeaf: false}},
		{1, HeaderCell{Name: "2025Q1", Depth: 1, IsLeaf: false}},
		{2, HeaderCell{Name: "2025M1", Depth: 2, IsLeaf: true}},
		{5, HeaderCell{Name: "2025Q2", Depth: 1, IsLeaf: false}},
	}
	for _, c := range checks {
		if res.RowHeaders[c.idx] != c.want {
			t.Errorf("header[%d] = %+v, want %+v", c.idx, res.RowHeaders[c.idx], c.want)
		}
	}
	wantVals := map[int]float64{0: 0, 1: 700, 2: 500, 3: 600, 4: 700, 5: 0}
	for idx, w := range wantVals {
		if !approxEq(res.Cells[idx][0], w) {
			t.Errorf("Cash at %s = %v, want %v", res.RowHeaders[idx].Name, res.Cells[idx][0], w)
		}
	}
}

func TestQueryICAxis(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()

	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "Global", Scenario: "Actual", Time: "2025M1"},
		Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}},
		Cols: []AxisSpec{{Dim: "IC", Member: model.NoneMember}, {Dim: "IC", Member: "DE"}},
	})
	if err != nil {
		t.Fatalf("Query (IC members): %v", err)
	}
	if !approxEq(res.Cells[0][0], 1400) || !approxEq(res.Cells[0][1], 0) {
		t.Errorf("IC axis cells = %v, want [1400 0]", res.Cells[0])
	}

	// IC expands against the Entity hierarchy.
	res, err = e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "Global", Scenario: "Actual", Time: "2025M1", Origin: model.OriginImport},
		Rows: []AxisSpec{{Dim: "IC", Member: "Global", Expand: ExpandTree}},
		Cols: []AxisSpec{{Dim: "Account", Member: "Sales"}},
	})
	if err != nil {
		t.Fatalf("Query (IC tree): %v", err)
	}
	wantNames := []string{"Global", "US", "DE", "FR"}
	if len(res.RowHeaders) != len(wantNames) {
		t.Fatalf("IC tree headers = %+v, want %v", res.RowHeaders, wantNames)
	}
	wantVals := []float64{0, 0, 200, 0}
	for i := range wantNames {
		if res.RowHeaders[i].Name != wantNames[i] {
			t.Errorf("IC header %d = %q, want %q", i, res.RowHeaders[i].Name, wantNames[i])
		}
		if !approxEq(res.Cells[i][0], wantVals[i]) {
			t.Errorf("IC=%s Sales = %v, want %v", wantNames[i], res.Cells[i][0], wantVals[i])
		}
	}
}

func TestQueryEmptyAxes(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()

	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M1"},
		Cols: []AxisSpec{{Dim: "Account", Member: "Sales"}},
	})
	if err != nil {
		t.Fatalf("Query (no rows): %v", err)
	}
	if len(res.RowHeaders) != 0 || len(res.Cells) != 0 || len(res.ColHeaders) != 1 {
		t.Errorf("no-rows result = %+v, want empty rows/cells, 1 col", res)
	}

	res, err = e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M1"},
		Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}},
	})
	if err != nil {
		t.Fatalf("Query (no cols): %v", err)
	}
	if len(res.Cells) != 1 || len(res.Cells[0]) != 0 {
		t.Errorf("no-cols cells = %+v, want one empty row", res.Cells)
	}
}

func TestQueryErrors(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	base := POV{Entity: "US", Scenario: "Actual", Time: "2025M1"}

	tests := []struct {
		name    string
		req     QueryRequest
		wantSub string
	}{
		{
			"unknown axis dim",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "Bogus", Member: "X"}}},
			"Bogus",
		},
		{
			"unknown account member",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "Account", Member: "Nope"}}},
			"Nope",
		},
		{
			"unknown member on cols",
			QueryRequest{Cube: "Main", POV: base,
				Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}},
				Cols: []AxisSpec{{Dim: "Time", Member: "2031M1"}}},
			"2031M1",
		},
		{
			"bad expand mode",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "Account", Member: "Sales", Expand: "explode"}}},
			"explode",
		},
		{
			"unknown view member",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "View", Member: "Quarterly"}}},
			"Quarterly",
		},
		{
			"unknown stage member",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "Stage", Member: "Weird"}}},
			"Weird",
		},
		{
			"unknown IC member",
			QueryRequest{Cube: "Main", POV: base, Rows: []AxisSpec{{Dim: "IC", Member: "Nobody"}}},
			"Nobody",
		},
		{
			"unknown cube",
			QueryRequest{Cube: "NoCube", POV: base, Rows: []AxisSpec{{Dim: "Account", Member: "Sales"}}},
			"NoCube",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.Query(meta, tc.req)
			if err == nil {
				t.Fatalf("Query(%+v) succeeded, want error mentioning %q", tc.req, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestQueryChildrenOfLeafIsEmpty(t *testing.T) {
	meta := newTestMeta(t)
	e := newTestEngine()
	res, err := e.Query(meta, QueryRequest{
		Cube: "Main",
		POV:  POV{Entity: "US", Scenario: "Actual", Time: "2025M1"},
		Rows: []AxisSpec{{Dim: "Account", Member: "Cash", Expand: ExpandChildren}},
		Cols: []AxisSpec{{Dim: "Time", Member: "2025M1"}},
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.RowHeaders) != 0 || len(res.Cells) != 0 {
		t.Errorf("children of leaf = %+v, want empty axis", res.RowHeaders)
	}
}

func TestJSONShapes(t *testing.T) {
	res := QueryResult{
		RowHeaders: []HeaderCell{{Name: "Sales", Depth: 1, IsLeaf: true}},
		ColHeaders: []HeaderCell{},
		Cells:      [][]float64{{1.5}},
	}
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal QueryResult: %v", err)
	}
	for _, key := range []string{`"rowHeaders"`, `"colHeaders"`, `"cells"`, `"name"`, `"depth"`, `"isLeaf"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("QueryResult JSON %s missing key %s", b, key)
		}
	}

	req := QueryRequest{Cube: "Main", POV: POV{Entity: "US"}, Rows: []AxisSpec{{Dim: "Account", Member: "Sales", Expand: "tree"}}}
	b, err = json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal QueryRequest: %v", err)
	}
	for _, key := range []string{`"cube"`, `"pov"`, `"rows"`, `"cols"`, `"dim"`, `"member"`, `"expand"`, `"entity":"US"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("QueryRequest JSON %s missing %s", b, key)
		}
	}

	var rt QueryRequest
	if err := json.Unmarshal([]byte(`{"cube":"Main","pov":{"account":"Sales","ud1":"East"},"rows":[{"dim":"Time","member":"2025","expand":"tree"}]}`), &rt); err != nil {
		t.Fatalf("unmarshal QueryRequest: %v", err)
	}
	if rt.POV.Account != "Sales" || rt.POV.UD1 != "East" || rt.Rows[0].Expand != "tree" {
		t.Errorf("round-tripped request = %+v", rt)
	}
}
