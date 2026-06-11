package seed

import (
	"bytes"
	"encoding/json"
	"math"
	"testing"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/stage"
)

const eps = 1e-9

func almostEq(a, b float64) bool { return math.Abs(a-b) <= eps }

func mustBuild(t *testing.T) (*model.Metadata, *cube.Store, map[string]*stage.Profile) {
	t.Helper()
	meta, store, profiles, err := Build()
	if err != nil {
		t.Fatalf("Build() error: %v", err)
	}
	if meta == nil || store == nil || profiles == nil {
		t.Fatalf("Build() returned nil component: meta=%v store=%v profiles=%v", meta, store, profiles)
	}
	return meta, store, profiles
}

func TestBuildValid(t *testing.T) {
	meta, _, _ := mustBuild(t)
	if probs := meta.Validate(); len(probs) != 0 {
		t.Errorf("metadata.Validate() problems: %v", probs)
	}
	c, err := meta.CubeOf(CubeName)
	if err != nil {
		t.Fatalf("CubeOf(%q): %v", CubeName, err)
	}
	if c.GroupCurrency != "USD" {
		t.Errorf("cube group currency = %q, want USD", c.GroupCurrency)
	}
	if c.Description != "GolfTrickle Inc demo" {
		t.Errorf("cube description = %q, want %q", c.Description, "GolfTrickle Inc demo")
	}
	if got := meta.Currencies(); len(got) != 3 || got[0] != "CAD" || got[1] != "EUR" || got[2] != "USD" {
		t.Errorf("Currencies() = %v, want [CAD EUR USD]", got)
	}
}

func TestEntityTree(t *testing.T) {
	meta, _, _ := mustBuild(t)
	ent := meta.Entity()
	tests := []struct {
		name      string
		parent    string
		currency  string
		ownership float64
		leaf      bool
	}{
		{"GolfTrickle Inc", "", "USD", 100, false},
		{"North America", "GolfTrickle Inc", "USD", 100, false},
		{"US Operations", "North America", "USD", 100, true},
		{"Canada", "North America", "CAD", 100, true},
		{"Europe", "GolfTrickle Inc", "EUR", 100, false},
		{"Germany", "Europe", "EUR", 100, true},
		{"France", "Europe", "EUR", 80, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := ent.Get(tc.name)
			if m == nil {
				t.Fatalf("entity %q missing", tc.name)
			}
			if m.Parent != tc.parent {
				t.Errorf("parent = %q, want %q", m.Parent, tc.parent)
			}
			if m.Currency != tc.currency {
				t.Errorf("currency = %q, want %q", m.Currency, tc.currency)
			}
			if !almostEq(m.OwnershipPct, tc.ownership) {
				t.Errorf("ownership = %v, want %v", m.OwnershipPct, tc.ownership)
			}
			if ent.IsLeaf(tc.name) != tc.leaf {
				t.Errorf("IsLeaf = %v, want %v", ent.IsLeaf(tc.name), tc.leaf)
			}
		})
	}
	if len(ent.Roots) != 1 || ent.Roots[0] != "GolfTrickle Inc" {
		t.Errorf("entity roots = %v, want [GolfTrickle Inc]", ent.Roots)
	}
	if got := len(ent.Leaves("GolfTrickle Inc")); got != 4 {
		t.Errorf("leaf entity count = %d, want 4", got)
	}
}

func TestAccountTree(t *testing.T) {
	meta, _, _ := mustBuild(t)
	acc := meta.Account()
	tests := []struct {
		name    string
		parent  string
		weight  float64
		typ     model.AccountType
		isIC    bool
		dynamic bool
		formula string
	}{
		{"NetIncome", "", 1, model.AccountRevenue, false, false, ""},
		{"GrossProfit", "NetIncome", 1, model.AccountRevenue, false, false, "A#Sales - A#COGS"},
		{"OpEx", "NetIncome", -1, model.AccountExpense, false, false, ""},
		{"Salaries", "OpEx", 1, model.AccountExpense, false, false, ""},
		{"Marketing", "OpEx", 1, model.AccountExpense, false, false, ""},
		{"Rent", "OpEx", 1, model.AccountExpense, false, false, ""},
		{"Sales", "", 1, model.AccountRevenue, true, false, ""},
		{"COGS", "", 1, model.AccountExpense, true, false, ""},
		{"BalanceSheet", "", 1, model.AccountAsset, false, false, ""},
		{"Cash", "BalanceSheet", 1, model.AccountAsset, false, false, ""},
		{"Receivables", "BalanceSheet", 1, model.AccountAsset, false, false, ""},
		{"Payables", "BalanceSheet", 1, model.AccountLiability, false, false, ""},
		{"GPMargin", "", 1, model.AccountNonFinancial, false, true,
			"IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := acc.Get(tc.name)
			if m == nil {
				t.Fatalf("account %q missing", tc.name)
			}
			if m.Parent != tc.parent {
				t.Errorf("parent = %q, want %q", m.Parent, tc.parent)
			}
			if !almostEq(m.Weight, tc.weight) {
				t.Errorf("weight = %v, want %v", m.Weight, tc.weight)
			}
			if m.AccountType != tc.typ {
				t.Errorf("type = %q, want %q", m.AccountType, tc.typ)
			}
			if m.IsIC != tc.isIC {
				t.Errorf("isIC = %v, want %v", m.IsIC, tc.isIC)
			}
			if m.DynamicCalc != tc.dynamic {
				t.Errorf("dynamicCalc = %v, want %v", m.DynamicCalc, tc.dynamic)
			}
			if m.Formula != tc.formula {
				t.Errorf("formula = %q, want %q", m.Formula, tc.formula)
			}
		})
	}
	wantRoots := []string{"NetIncome", "Sales", "COGS", "BalanceSheet", "GPMargin"}
	if len(acc.Roots) != len(wantRoots) {
		t.Fatalf("account roots = %v, want %v", acc.Roots, wantRoots)
	}
	for i, r := range wantRoots {
		if acc.Roots[i] != r {
			t.Errorf("account root[%d] = %q, want %q", i, acc.Roots[i], r)
		}
	}
	// Sales/COGS must NOT be descendants of NetIncome.
	for _, name := range []string{"Sales", "COGS"} {
		if acc.IsAncestor("NetIncome", name) {
			t.Errorf("%s must not be under NetIncome", name)
		}
		if !acc.IsLeaf(name) {
			t.Errorf("%s must be a leaf", name)
		}
	}
}

func TestScenariosAndTime(t *testing.T) {
	meta, _, _ := mustBuild(t)
	sc := meta.Dim(model.DimScenario)
	for _, s := range []string{ScenarioActual, ScenarioBudget} {
		if !sc.Has(s) || !sc.IsLeaf(s) || sc.Get(s).Parent != "" {
			t.Errorf("scenario %q should be a flat root leaf", s)
		}
	}
	if len(sc.Roots) != 2 {
		t.Errorf("scenario roots = %v, want 2", sc.Roots)
	}
	years := model.TimeYears(meta.Dim(model.DimTime))
	want := []int{2024, 2025, 2026}
	if len(years) != len(want) {
		t.Fatalf("time years = %v, want %v", years, want)
	}
	for i := range want {
		if years[i] != want[i] {
			t.Errorf("year[%d] = %d, want %d", i, years[i], want[i])
		}
	}
	for _, m := range []string{"2024M1", "2025M7", "2026M12", "2025Q3"} {
		if !meta.Dim(model.DimTime).Has(m) {
			t.Errorf("time member %q missing", m)
		}
	}
}

func TestRatesComplete(t *testing.T) {
	meta, _, _ := mustBuild(t)
	bases := []struct {
		currency string
		typ      model.RateType
		base     float64
	}{
		{"CAD", model.RateAverage, 0.74},
		{"CAD", model.RateClosing, 0.73},
		{"EUR", model.RateAverage, 1.09},
		{"EUR", model.RateClosing, 1.08},
	}
	for _, sc := range []string{ScenarioActual, ScenarioBudget} {
		for m := 1; m <= 12; m++ {
			tm := model.TimeName(2025, m)
			for _, b := range bases {
				v, ok := meta.Rates.Get(sc, tm, b.currency, b.typ)
				if !ok {
					t.Errorf("missing rate %s/%s %s %s", sc, tm, b.currency, b.typ)
					continue
				}
				want := b.base + 0.001*float64(m-1)
				if !almostEq(v, want) {
					t.Errorf("rate %s/%s %s %s = %v, want %v", sc, tm, b.currency, b.typ, v, want)
				}
			}
		}
	}
	if got := len(meta.Rates); got != 96 {
		t.Errorf("rate entries = %d, want 96 (2 scenarios × 12 months × 2 currencies × 2 types)", got)
	}
	// Months must differ (drift).
	v1, _ := meta.Rates.Get(ScenarioActual, "2025M1", "EUR", model.RateAverage)
	v2, _ := meta.Rates.Get(ScenarioActual, "2025M2", "EUR", model.RateAverage)
	if almostEq(v1, v2) {
		t.Errorf("rates for adjacent months should differ, got %v and %v", v1, v2)
	}
}

func TestInputCellsReferenceValidLeaves(t *testing.T) {
	meta, store, _ := mustBuild(t)
	timeDim := meta.Dim(model.DimTime)
	leafDims := []struct {
		dim model.DimType
		get func(cube.CellCoord) string
	}{
		{model.DimAccount, func(c cube.CellCoord) string { return c.Account }},
		{model.DimFlow, func(c cube.CellCoord) string { return c.Flow }},
		{model.DimUD1, func(c cube.CellCoord) string { return c.UD1 }},
		{model.DimUD2, func(c cube.CellCoord) string { return c.UD2 }},
		{model.DimUD3, func(c cube.CellCoord) string { return c.UD3 }},
		{model.DimUD4, func(c cube.CellCoord) string { return c.UD4 }},
	}
	totalCells := 0
	for uk, u := range store.Units {
		if uk.Cube != CubeName {
			t.Errorf("unit %s: cube = %q, want %q", uk.Key(), uk.Cube, CubeName)
		}
		if !meta.Entity().IsLeaf(uk.Entity) {
			t.Errorf("unit %s: entity %q is not a leaf entity", uk.Key(), uk.Entity)
		}
		if !meta.Dim(model.DimScenario).IsLeaf(uk.Scenario) {
			t.Errorf("unit %s: scenario %q is not a leaf scenario", uk.Key(), uk.Scenario)
		}
		if !model.TimeIsMonth(uk.Time) || !timeDim.Has(uk.Time) {
			t.Errorf("unit %s: time %q is not a valid month member", uk.Key(), uk.Time)
		}
		if len(u.Stages) != 0 {
			t.Errorf("unit %s: seed must not materialize stages, got %d", uk.Key(), len(u.Stages))
		}
		for coord, v := range u.Input {
			totalCells++
			if coord.Origin != model.OriginImport {
				t.Errorf("unit %s cell %s: origin = %q, want Import", uk.Key(), coord.Key(), coord.Origin)
			}
			for _, ld := range leafDims {
				name := ld.get(coord)
				d := meta.Dim(ld.dim)
				if !d.Has(name) || !d.IsLeaf(name) {
					t.Errorf("unit %s cell %s: %s %q is not an existing leaf", uk.Key(), coord.Key(), ld.dim, name)
				}
			}
			if a := meta.Account().Get(coord.Account); a != nil && (a.DynamicCalc || a.Formula != "") {
				t.Errorf("unit %s cell %s: input written to calc account %q", uk.Key(), coord.Key(), coord.Account)
			}
			if !meta.ValidIC(coord.IC) || coord.IC == "" {
				t.Errorf("unit %s cell %s: invalid IC %q", uk.Key(), coord.Key(), coord.IC)
			}
			if coord.IC != model.NoneMember && !meta.Entity().Has(coord.IC) {
				t.Errorf("unit %s cell %s: IC partner %q is not an entity", uk.Key(), coord.Key(), coord.IC)
			}
			if v == 0 {
				t.Errorf("unit %s cell %s: zero value stored", uk.Key(), coord.Key())
			}
		}
	}
	// 4 leaf entities × (6 Actual + 12 Budget) months.
	if got := len(store.Units); got != 72 {
		t.Errorf("unit count = %d, want 72", got)
	}
	// 72 units × 8 base cells + 18 scenario-months × 2 IC cells.
	if totalCells != 612 {
		t.Errorf("total input cells = %d, want 612", totalCells)
	}
}

func TestSeededMonthsPerScenario(t *testing.T) {
	_, store, _ := mustBuild(t)
	entities := []string{"US Operations", "Canada", "Germany", "France"}
	scenarios := []struct {
		name   string
		months int
	}{
		{ScenarioActual, 6},
		{ScenarioBudget, 12},
	}
	for _, sc := range scenarios {
		for _, e := range entities {
			for m := 1; m <= 12; m++ {
				uk := cube.UnitKey{Cube: CubeName, Entity: e, Scenario: sc.name, Time: model.TimeName(2025, m)}
				u := store.Unit(uk)
				if m <= sc.months {
					if u == nil || len(u.Input) == 0 {
						t.Errorf("missing unit data for %s", uk.Key())
					}
				} else if u != nil {
					t.Errorf("unexpected unit %s beyond seeded months", uk.Key())
				}
			}
		}
	}
}

func TestSpotAmounts(t *testing.T) {
	_, store, _ := mustBuild(t)
	tests := []struct {
		entity, scenario, tm, account string
		want                          float64
	}{
		{"US Operations", "Actual", "2025M1", "Sales", 1000},
		{"US Operations", "Actual", "2025M1", "COGS", 580},
		{"US Operations", "Actual", "2025M3", "Sales", 1050},
		{"US Operations", "Actual", "2025M1", "Cash", 5000},
		{"US Operations", "Actual", "2025M4", "Cash", 5300},
		{"US Operations", "Budget", "2025M1", "Sales", 1050},
		{"Canada", "Actual", "2025M1", "Sales", 600},
		{"Canada", "Actual", "2025M2", "Salaries", 143},
		{"Canada", "Actual", "2025M2", "Rent", 30},
		{"Germany", "Actual", "2025M1", "Sales", 850},
		{"Germany", "Actual", "2025M4", "Cash", 3500},
		{"Germany", "Actual", "2025M1", "COGS", 510},
		{"France", "Actual", "2025M1", "Payables", 350},
		{"France", "Budget", "2025M12", "Rent", 26.25},
	}
	for _, tc := range tests {
		uk := cube.UnitKey{Cube: CubeName, Entity: tc.entity, Scenario: tc.scenario, Time: tc.tm}
		u := store.Unit(uk)
		if u == nil {
			t.Errorf("%s: unit missing", uk.Key())
			continue
		}
		got := u.Input[importCoord(tc.account, "")]
		if !almostEq(got, tc.want) {
			t.Errorf("%s %s = %v, want %v", uk.Key(), tc.account, got, tc.want)
		}
	}
}

func TestCOGSRatio(t *testing.T) {
	_, store, _ := mustBuild(t)
	for uk, u := range store.Units {
		sales := u.Input[importCoord("Sales", "")]
		cogs := u.Input[importCoord("COGS", "")]
		if sales == 0 {
			t.Errorf("unit %s: no base Sales cell", uk.Key())
			continue
		}
		ratio := cogs / sales
		if ratio < 0.55-1e-3 || ratio > 0.65+1e-3 {
			t.Errorf("unit %s: COGS/Sales = %v, want within [0.55, 0.65]", uk.Key(), ratio)
		}
	}
}

func TestICPairAllMonths(t *testing.T) {
	_, store, _ := mustBuild(t)
	scenarios := []struct {
		name   string
		months int
	}{
		{ScenarioActual, 6},
		{ScenarioBudget, 12},
	}
	for _, sc := range scenarios {
		for m := 1; m <= sc.months; m++ {
			tm := model.TimeName(2025, m)
			wantUSD := 200 + 10*float64(m-1)
			wantEUR := math.Round(wantUSD/1.09*100) / 100

			us := store.Unit(cube.UnitKey{Cube: CubeName, Entity: "US Operations", Scenario: sc.name, Time: tm})
			if us == nil {
				t.Fatalf("%s/%s: US Operations unit missing", sc.name, tm)
			}
			got, ok := us.Input[importCoord("Sales", "Germany")]
			if !ok {
				t.Errorf("%s/%s: missing US Operations Sales[IC=Germany]", sc.name, tm)
			} else if !almostEq(got, wantUSD) {
				t.Errorf("%s/%s: Sales[IC=Germany] = %v, want %v", sc.name, tm, got, wantUSD)
			}

			de := store.Unit(cube.UnitKey{Cube: CubeName, Entity: "Germany", Scenario: sc.name, Time: tm})
			if de == nil {
				t.Fatalf("%s/%s: Germany unit missing", sc.name, tm)
			}
			got, ok = de.Input[importCoord("COGS", "US Operations")]
			if !ok {
				t.Errorf("%s/%s: missing Germany COGS[IC=US Operations]", sc.name, tm)
			} else if !almostEq(got, wantEUR) {
				t.Errorf("%s/%s: COGS[IC=US Operations] = %v, want %v", sc.name, tm, got, wantEUR)
			}
		}
	}
	// Spot-check the exact rounded EUR value for month 1: 200/1.09 = 183.49.
	de := store.Unit(cube.UnitKey{Cube: CubeName, Entity: "Germany", Scenario: ScenarioActual, Time: "2025M1"})
	if got := de.Input[importCoord("COGS", "US Operations")]; !almostEq(got, 183.49) {
		t.Errorf("Germany COGS[IC=US Operations] 2025M1 = %v, want 183.49", got)
	}
}

func TestDeterminism(t *testing.T) {
	meta1, store1, prof1, err := Build()
	if err != nil {
		t.Fatalf("first Build(): %v", err)
	}
	meta2, store2, prof2, err := Build()
	if err != nil {
		t.Fatalf("second Build(): %v", err)
	}
	marshal := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return b
	}
	if !bytes.Equal(marshal(store1), marshal(store2)) {
		t.Error("two Build() calls produced different store JSON")
	}
	if !bytes.Equal(marshal(meta1), marshal(meta2)) {
		t.Error("two Build() calls produced different metadata JSON")
	}
	if !bytes.Equal(marshal(prof1), marshal(prof2)) {
		t.Error("two Build() calls produced different profile JSON")
	}
}

func TestProfileShape(t *testing.T) {
	_, _, profiles, err := Build()
	if err != nil {
		t.Fatalf("Build(): %v", err)
	}
	if len(profiles) != 1 {
		t.Fatalf("profiles = %d, want 1", len(profiles))
	}
	p := profiles[ProfileName]
	if p == nil {
		t.Fatalf("profile %q missing", ProfileName)
	}
	if p.Name != ProfileName || p.Cube != CubeName || !p.HasHeader || p.Delimiter != "," || p.AmountCol != 3 {
		t.Errorf("profile header fields wrong: %+v", p)
	}
	colTests := []struct {
		dim   model.DimType
		col   int
		fixed string
	}{
		{model.DimEntity, 0, ""},
		{model.DimAccount, 1, ""},
		{model.DimTime, 2, ""},
		{model.DimScenario, -1, "Actual"},
	}
	for _, tc := range colTests {
		spec, ok := p.Columns[tc.dim]
		if !ok {
			t.Errorf("missing column spec for %s", tc.dim)
			continue
		}
		if spec.Col != tc.col || spec.Fixed != tc.fixed {
			t.Errorf("column %s = %+v, want col=%d fixed=%q", tc.dim, spec, tc.col, tc.fixed)
		}
	}
	if len(p.Rules) != 3 {
		t.Fatalf("rules = %d, want 3", len(p.Rules))
	}
	wantRules := []stage.Rule{
		{Dim: model.DimAccount, Kind: stage.KindExact, Src: "4100", Target: "Sales"},
		{Dim: model.DimAccount, Kind: stage.KindPrefix, Src: "5*", Target: "COGS"},
		{Dim: model.DimEntity, Kind: stage.KindDefault, Src: "*", Target: "US Operations"},
	}
	for i, want := range wantRules {
		if p.Rules[i] != want {
			t.Errorf("rule[%d] = %+v, want %+v", i, p.Rules[i], want)
		}
	}
}

func TestSampleCSVImportsCleanly(t *testing.T) {
	meta, _, profiles := mustBuild(t)
	p := profiles[ProfileName]
	res, err := stage.Transform(p, SampleCSV())
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	stage.Validate(meta, res)
	if len(res.Issues) != 0 {
		t.Errorf("global issues = %v, want none", res.Issues)
	}
	if len(res.Rows) != 6 {
		t.Fatalf("rows = %d, want 6", len(res.Rows))
	}
	wantAccounts := []string{"Sales", "COGS", "Sales", "COGS", "Sales", "COGS"}
	for i, row := range res.Rows {
		if len(row.Issues) != 0 {
			t.Errorf("row %d (line %d) issues = %v, want none", i, row.Line, row.Issues)
		}
		if row.Entity != "US Operations" {
			t.Errorf("row %d entity = %q, want US Operations (default rule)", i, row.Entity)
		}
		if row.Account != wantAccounts[i] {
			t.Errorf("row %d account = %q, want %q", i, row.Account, wantAccounts[i])
		}
		if row.Scenario != ScenarioActual {
			t.Errorf("row %d scenario = %q, want Actual", i, row.Scenario)
		}
		if row.Time != "2025M7" {
			t.Errorf("row %d time = %q, want 2025M7", i, row.Time)
		}
	}

	plan := stage.LoadPlan(res)
	if len(plan) != 1 {
		t.Fatalf("load plan units = %d, want 1", len(plan))
	}
	uk := cube.UnitKey{Cube: CubeName, Entity: "US Operations", Scenario: ScenarioActual, Time: "2025M7"}
	writes, ok := plan[uk]
	if !ok {
		t.Fatalf("load plan missing unit %s; got %v", uk.Key(), plan)
	}
	got := map[string]float64{}
	for _, w := range writes {
		if w.Coord.Origin != model.OriginImport {
			t.Errorf("write origin = %q, want Import", w.Coord.Origin)
		}
		got[w.Coord.Account] = w.Value
	}
	if !almostEq(got["Sales"], 3200) {
		t.Errorf("planned Sales = %v, want 3200 (1500+720+980)", got["Sales"])
	}
	if !almostEq(got["COGS"], 1615) {
		t.Errorf("planned COGS = %v, want 1615 (870+445+300)", got["COGS"])
	}
}
