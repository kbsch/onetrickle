package consol_test

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"onetrickle/internal/consol"
	"onetrickle/internal/cube"
	"onetrickle/internal/model"
)

const (
	tCube  = "Test"
	tScen  = "Actual"
	tMonth = "2025M1"
)

const epsTest = 1e-9

func approx(a, b float64) bool { return math.Abs(a-b) <= epsTest }

// ---- fixture builders ----

func newMeta(t *testing.T) *model.Metadata {
	t.Helper()
	m := model.NewMetadata()
	m.Cubes[tCube] = &model.Cube{Name: tCube, GroupCurrency: "USD"}
	model.AddTimeYear(m.Dims[model.DimTime], 2025)
	addMember(t, m.Dims[model.DimScenario], &model.Member{Name: tScen})
	return m
}

func addMember(t *testing.T, d *model.Dimension, mem *model.Member) {
	t.Helper()
	if err := d.AddMember(mem); err != nil {
		t.Fatalf("add member %q to %s: %v", mem.Name, d.Type, err)
	}
}

func addEntity(t *testing.T, m *model.Metadata, name, parent, currency string, pct float64) {
	t.Helper()
	addMember(t, m.Entity(), &model.Member{Name: name, Parent: parent, Currency: currency, OwnershipPct: pct})
}

func addAccount(t *testing.T, m *model.Metadata, name string, typ model.AccountType, isIC bool, formula string) {
	t.Helper()
	addMember(t, m.Account(), &model.Member{Name: name, AccountType: typ, IsIC: isIC, Formula: formula})
}

func setEURRates(t *testing.T, m *model.Metadata) {
	t.Helper()
	if err := m.Rates.Set(tScen, tMonth, "EUR", model.RateAverage, 1.10); err != nil {
		t.Fatalf("set average rate: %v", err)
	}
	if err := m.Rates.Set(tScen, tMonth, "EUR", model.RateClosing, 1.08); err != nil {
		t.Fatalf("set closing rate: %v", err)
	}
}

func unitKey(entity string) cube.UnitKey {
	return cube.UnitKey{Cube: tCube, Entity: entity, Scenario: tScen, Time: tMonth}
}

// put writes one Input cell (coord normalized, default UD/Flow = None).
func put(s *cube.Store, entity, account, origin, ic string, v float64) {
	u := s.Ensure(unitKey(entity))
	c := cube.CellCoord{Account: account, Origin: origin, IC: ic}.Normalize()
	u.Input[c] = v
}

// fixtureA builds the SPEC §5 gold fixture A.
func fixtureA(t *testing.T) (*model.Metadata, *cube.Store) {
	t.Helper()
	m := newMeta(t)
	addEntity(t, m, "Global", "", "USD", 0)
	addEntity(t, m, "US", "Global", "USD", 0)
	addEntity(t, m, "DE", "Global", "EUR", 0)
	addAccount(t, m, "Sales", model.AccountRevenue, true, "")
	addAccount(t, m, "COGS", model.AccountExpense, true, "")
	addAccount(t, m, "Cash", model.AccountAsset, false, "")
	setEURRates(t, m)

	s := cube.NewStore()
	put(s, "US", "Sales", model.OriginImport, "", 1000)
	put(s, "US", "Sales", model.OriginImport, "DE", 200)
	put(s, "US", "Cash", model.OriginImport, "", 500)
	put(s, "DE", "Sales", model.OriginImport, "", 400)
	put(s, "DE", "COGS", model.OriginImport, "US", 180)
	put(s, "DE", "Cash", model.OriginImport, "", 300)
	return m, s
}

// fixtureB builds the SPEC §5 gold fixture B (DE 80% owned, no IC cells).
func fixtureB(t *testing.T) (*model.Metadata, *cube.Store) {
	t.Helper()
	m := newMeta(t)
	addEntity(t, m, "Global", "", "USD", 0)
	addEntity(t, m, "US", "Global", "USD", 100)
	addEntity(t, m, "DE", "Global", "EUR", 80)
	addAccount(t, m, "Sales", model.AccountRevenue, true, "")
	addAccount(t, m, "COGS", model.AccountExpense, true, "")
	addAccount(t, m, "Cash", model.AccountAsset, false, "")
	setEURRates(t, m)

	s := cube.NewStore()
	put(s, "US", "Sales", model.OriginImport, "", 1000)
	put(s, "US", "Cash", model.OriginImport, "", 500)
	put(s, "DE", "Sales", model.OriginImport, "", 400)
	put(s, "DE", "Cash", model.OriginImport, "", 300)
	return m, s
}

type cellCase struct {
	name string
	pov  cube.POV
	want float64
}

func checkCells(t *testing.T, m *model.Metadata, s *cube.Store, cases []cellCase) {
	t.Helper()
	eng := cube.NewEngine(s)
	for _, tc := range cases {
		pov := tc.pov
		if pov.Cube == "" {
			pov.Cube = tCube
		}
		if pov.Scenario == "" {
			pov.Scenario = tScen
		}
		if pov.Time == "" {
			pov.Time = tMonth
		}
		got := eng.GetCell(m, pov)
		if !approx(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// ---- tests ----

func TestProcessGoldFixtureA(t *testing.T) {
	m, s := fixtureA(t)
	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", res.Issues)
	}
	if res.Cube != tCube || res.Scenario != tScen || res.Time != tMonth {
		t.Errorf("Result identity = %s/%s/%s", res.Cube, res.Scenario, res.Time)
	}
	if res.UnitsProcessed != 3 {
		t.Errorf("UnitsProcessed = %d, want 3 (US, DE, Global)", res.UnitsProcessed)
	}
	// translated 3+3, elim 2, consolidated 3+3+6.
	if res.CellsWritten != 20 {
		t.Errorf("CellsWritten = %d, want 20", res.CellsWritten)
	}

	checkCells(t, m, s, []cellCase{
		{"US Sales", cube.POV{Entity: "US", Account: "Sales"}, 1200},
		{"US Cash", cube.POV{Entity: "US", Account: "Cash"}, 500},
		{"DE Sales", cube.POV{Entity: "DE", Account: "Sales"}, 440},
		{"DE COGS", cube.POV{Entity: "DE", Account: "COGS"}, 198},
		{"DE Cash", cube.POV{Entity: "DE", Account: "Cash"}, 324},
		{"Global Sales", cube.POV{Entity: "Global", Account: "Sales"}, 1440},
		{"Global COGS", cube.POV{Entity: "Global", Account: "COGS"}, 0},
		{"Global Cash", cube.POV{Entity: "Global", Account: "Cash"}, 824},
		{"Global Elim Sales IC=DE", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination", IC: "DE"}, -200},
		{"Global Elim COGS IC=US", cube.POV{Entity: "Global", Account: "COGS", Stage: "Elimination", IC: "US"}, -198},
		{"Global Elim Sales total", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination"}, -200},
		{"Global Elim Sales IC=US", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination", IC: "US"}, 0},
		{"US Elim empty", cube.POV{Entity: "US", Account: "Sales", Stage: "Elimination"}, 0},
		{"DE Translated Sales", cube.POV{Entity: "DE", Account: "Sales", Stage: "Translated"}, 440},
	})
}

func TestProcessGoldFixtureB_OwnershipScaling(t *testing.T) {
	m, s := fixtureB(t)
	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", res.Issues)
	}
	if res.UnitsProcessed != 3 {
		t.Errorf("UnitsProcessed = %d, want 3", res.UnitsProcessed)
	}
	// translated 2+2, elim 0, consolidated 2+2+2.
	if res.CellsWritten != 10 {
		t.Errorf("CellsWritten = %d, want 10", res.CellsWritten)
	}

	checkCells(t, m, s, []cellCase{
		{"US Sales", cube.POV{Entity: "US", Account: "Sales"}, 1000},
		{"DE Sales", cube.POV{Entity: "DE", Account: "Sales"}, 440},
		{"DE Cash", cube.POV{Entity: "DE", Account: "Cash"}, 324},
		{"Global Sales", cube.POV{Entity: "Global", Account: "Sales"}, 1352},
		{"Global Cash", cube.POV{Entity: "Global", Account: "Cash"}, 759.2},
		{"Global Elim Sales", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination"}, 0},
	})
}

// TestProcessStoredCalc verifies stored calcs per tuple: subtree rollups with
// edge weights, cells stored AT non-leaf accounts, per-rest-tuple evaluation
// (distinct IC partners are distinct tuples), Origin=Calc placement, and
// downstream translation + consolidation of the calc cells.
func TestProcessStoredCalc(t *testing.T) {
	m := newMeta(t)
	addEntity(t, m, "Global", "", "USD", 0)
	addEntity(t, m, "US", "Global", "USD", 0)
	addEntity(t, m, "DE", "Global", "EUR", 0)
	addAccount(t, m, "TotalSales", model.AccountRevenue, false, "")
	addMember(t, m.Account(), &model.Member{Name: "Sales", Parent: "TotalSales", AccountType: model.AccountRevenue, Weight: 1})
	addMember(t, m.Account(), &model.Member{Name: "Returns", Parent: "TotalSales", AccountType: model.AccountRevenue, Weight: -1})
	addAccount(t, m, "COGS", model.AccountExpense, false, "")
	addAccount(t, m, "GrossProfit", model.AccountRevenue, false, "A#TotalSales - A#COGS")
	setEURRates(t, m)

	s := cube.NewStore()
	put(s, "US", "Sales", model.OriginImport, "", 1000)
	put(s, "US", "Sales", model.OriginImport, "DE", 200)
	put(s, "US", "Returns", model.OriginImport, "", 50)
	put(s, "US", "COGS", model.OriginImport, "", 300)
	put(s, "US", "TotalSales", model.OriginAdj, "", 10) // cell AT a non-leaf account
	put(s, "DE", "Sales", model.OriginImport, "", 400)
	put(s, "DE", "COGS", model.OriginImport, "", 180)

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", res.Issues)
	}

	// US tuple {IC=None}: TotalSales = 10 + 1000 - 50 = 960, GP = 660.
	// US tuple {IC=DE}:   TotalSales = 200, GP = 200.
	// DE tuple {IC=None}: GP = 400 - 180 = 220 -> translated 242.
	checkCells(t, m, s, []cellCase{
		{"US GP local total", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local"}, 860},
		{"US GP local IC=DE", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local", IC: "DE"}, 200},
		{"US GP local IC=None", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local", IC: "None"}, 660},
		{"US GP local origin Calc", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local", Origin: model.OriginCalc}, 860},
		{"US GP local origin Import", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local", Origin: model.OriginImport}, 0},
		{"US GP consolidated", cube.POV{Entity: "US", Account: "GrossProfit"}, 860},
		{"DE GP local", cube.POV{Entity: "DE", Account: "GrossProfit", Stage: "Local"}, 220},
		{"DE GP consolidated", cube.POV{Entity: "DE", Account: "GrossProfit"}, 242},
		{"Global GP consolidated", cube.POV{Entity: "Global", Account: "GrossProfit"}, 1102},
	})
}

// TestProcessChainedFormula verifies that a formula whose only data comes
// from another formula's freshly computed Calc cells (second-degree data)
// still evaluates: topo order computes GrossProfit before NetIncome.
func TestProcessChainedFormula(t *testing.T) {
	m := newMeta(t)
	addEntity(t, m, "Global", "", "USD", 0)
	addEntity(t, m, "US", "Global", "USD", 0)
	addAccount(t, m, "Sales", model.AccountRevenue, false, "")
	addAccount(t, m, "COGS", model.AccountExpense, false, "")
	addAccount(t, m, "OpEx", model.AccountExpense, false, "") // never has data
	addAccount(t, m, "GrossProfit", model.AccountRevenue, false, "A#Sales - A#COGS")
	addAccount(t, m, "NetIncome", model.AccountRevenue, false, "A#GrossProfit - A#OpEx")

	s := cube.NewStore()
	put(s, "US", "Sales", model.OriginImport, "", 1000)
	put(s, "US", "COGS", model.OriginImport, "", 300)

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if len(res.Issues) != 0 {
		t.Fatalf("Issues = %v, want none", res.Issues)
	}

	checkCells(t, m, s, []cellCase{
		{"US GP local", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local"}, 700},
		{"US NI local", cube.POV{Entity: "US", Account: "NetIncome", Stage: "Local"}, 700},
		{"US NI local origin Calc", cube.POV{Entity: "US", Account: "NetIncome", Stage: "Local", Origin: model.OriginCalc}, 700},
		{"US NI consolidated", cube.POV{Entity: "US", Account: "NetIncome"}, 700},
		{"Global NI consolidated", cube.POV{Entity: "Global", Account: "NetIncome"}, 700},
	})
}

// TestProcessStoredCalcParentRefOrdering pins the implicit dependency a
// formula takes on another formula through a PARENT-account reference: the
// helper formula reads A#Total whose subtree contains the GP formula account,
// so GP must evaluate first regardless of how the names sort.
func TestProcessStoredCalcParentRefOrdering(t *testing.T) {
	for _, helper := range []string{"AAA", "ZZZ"} { // sorts before and after "GP"
		t.Run("helper "+helper, func(t *testing.T) {
			m := newMeta(t)
			addEntity(t, m, "US", "", "USD", 0)
			addAccount(t, m, "Total", model.AccountRevenue, false, "")
			addMember(t, m.Account(), &model.Member{Name: "GP", Parent: "Total",
				AccountType: model.AccountRevenue, Formula: "A#Sales - A#COGS"})
			addMember(t, m.Account(), &model.Member{Name: "Other", Parent: "Total",
				AccountType: model.AccountRevenue})
			addAccount(t, m, "Sales", model.AccountRevenue, false, "")
			addAccount(t, m, "COGS", model.AccountExpense, false, "")
			addAccount(t, m, helper, model.AccountRevenue, false, "A#Total")

			s := cube.NewStore()
			put(s, "US", "Sales", model.OriginImport, "", 100)
			put(s, "US", "COGS", model.OriginImport, "", 40)
			put(s, "US", "Other", model.OriginImport, "", 5)

			if _, err := consol.Process(m, s, tCube, tScen, tMonth); err != nil {
				t.Fatalf("Process: %v", err)
			}
			checkCells(t, m, s, []cellCase{
				{"GP local", cube.POV{Entity: "US", Account: "GP", Stage: "Local", Origin: model.OriginCalc}, 60},
				// Other(5) + GP Calc(60) — wrong order used to yield 5 for AAA.
				{"helper local", cube.POV{Entity: "US", Account: helper, Stage: "Local", Origin: model.OriginCalc}, 65},
			})
		})
	}
}

// TestProcessStoredCalcAncestorCycle pins that a formula referencing an
// ANCESTOR of its own target account is reported as a cycle, not evaluated
// in some arbitrary order.
func TestProcessStoredCalcAncestorCycle(t *testing.T) {
	m := newMeta(t)
	addEntity(t, m, "US", "", "USD", 0)
	addAccount(t, m, "Total", model.AccountRevenue, false, "")
	addMember(t, m.Account(), &model.Member{Name: "Part", Parent: "Total",
		AccountType: model.AccountRevenue, Formula: "A#Total * 0.5"})
	addMember(t, m.Account(), &model.Member{Name: "Other", Parent: "Total",
		AccountType: model.AccountRevenue})

	s := cube.NewStore()
	put(s, "US", "Other", model.OriginImport, "", 10)

	_, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("Process err = %v, want formula cycle error", err)
	}
}

// TestProcessStoredCalcNonLeafTarget pins that stored-calc formulas must
// target leaf accounts (a Calc cell at a non-leaf double-counts with its
// children in every rollup).
func TestProcessStoredCalcNonLeafTarget(t *testing.T) {
	m := newMeta(t)
	addEntity(t, m, "US", "", "USD", 0)
	addAccount(t, m, "GP", model.AccountRevenue, false, "A#Sales - A#COGS")
	addMember(t, m.Account(), &model.Member{Name: "Sales", Parent: "GP", AccountType: model.AccountRevenue})
	addMember(t, m.Account(), &model.Member{Name: "COGS", Parent: "GP", Weight: -1, AccountType: model.AccountExpense})

	s := cube.NewStore()
	put(s, "US", "Sales", model.OriginImport, "", 100)

	_, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err == nil || !strings.Contains(err.Error(), "leaf") {
		t.Fatalf("Process err = %v, want non-leaf stored-calc error", err)
	}
}

// TestProcessStoredCalcNonFiniteSkipped pins that a formula overflowing to
// ±Inf records an Issue and writes nothing (non-finite values would poison
// JSON persistence).
func TestProcessStoredCalcNonFiniteSkipped(t *testing.T) {
	m := newMeta(t)
	addEntity(t, m, "US", "", "USD", 0)
	addAccount(t, m, "Big", model.AccountRevenue, false, "")
	addAccount(t, m, "Boom", model.AccountRevenue, false, "A#Big * A#Big")

	s := cube.NewStore()
	put(s, "US", "Big", model.OriginImport, "", 1e308)

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	found := false
	for _, is := range res.Issues {
		if strings.Contains(is, "non-finite") && strings.Contains(is, "Boom") {
			found = true
		}
	}
	if !found {
		t.Errorf("Issues = %v, want a non-finite issue for Boom", res.Issues)
	}
	checkCells(t, m, s, []cellCase{
		{"Boom local Calc", cube.POV{Entity: "US", Account: "Boom", Stage: "Local", Origin: model.OriginCalc}, 0},
	})
}

func TestProcessMissingRateIssues(t *testing.T) {
	m, s := fixtureA(t)
	m.Rates = model.RateTable{} // drop all rates

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	want := []string{
		"missing Average rate for EUR Actual/2025M1",
		"missing Closing rate for EUR Actual/2025M1",
	}
	if !reflect.DeepEqual(res.Issues, want) {
		t.Fatalf("Issues = %v, want %v", res.Issues, want)
	}

	// Fallback rate 1.0 everywhere.
	checkCells(t, m, s, []cellCase{
		{"DE Sales (rate 1)", cube.POV{Entity: "DE", Account: "Sales"}, 400},
		{"DE Cash (rate 1)", cube.POV{Entity: "DE", Account: "Cash"}, 300},
		{"Global Cash", cube.POV{Entity: "Global", Account: "Cash"}, 800},
		{"Global Sales", cube.POV{Entity: "Global", Account: "Sales"}, 1400},
		{"Global Elim COGS IC=US", cube.POV{Entity: "Global", Account: "COGS", Stage: "Elimination", IC: "US"}, -180},
	})
}

func TestProcessFCAMiss(t *testing.T) {
	m, s := fixtureA(t)
	addEntity(t, m, "Mars", "", "USD", 0) // separate root: no common ancestor with US
	put(s, "US", "Sales", model.OriginImport, "Mars", 100)

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	want := []string{`no common ancestor for entity "US" and IC partner "Mars"`}
	if !reflect.DeepEqual(res.Issues, want) {
		t.Fatalf("Issues = %v, want %v", res.Issues, want)
	}

	checkCells(t, m, s, []cellCase{
		// The Mars-partner cell is NOT eliminated; the DE pair still is.
		{"Global Sales", cube.POV{Entity: "Global", Account: "Sales"}, 1540},
		{"Global Elim Sales total", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination"}, -200},
		{"Global Elim Sales IC=Mars", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination", IC: "Mars"}, 0},
		{"Mars Elim Sales", cube.POV{Entity: "Mars", Account: "Sales", Stage: "Elimination"}, 0},
		{"Mars consolidated Sales", cube.POV{Entity: "Mars", Account: "Sales"}, 0},
	})
	if res.UnitsProcessed != 3 {
		t.Errorf("UnitsProcessed = %d, want 3 (Mars untouched)", res.UnitsProcessed)
	}
}

// TestProcessIdempotent runs Process twice over a fixture with a stored calc
// and pre-seeded stale engine artifacts; both runs must return equal Results
// and identical cell values, proving Calc origin and all stages are rebuilt.
func TestProcessIdempotent(t *testing.T) {
	m, s := fixtureA(t)
	addAccount(t, m, "GrossProfit", model.AccountRevenue, false, "A#Sales - A#COGS")

	// Stale garbage that a previous (hypothetical) run could have left.
	put(s, "US", "GrossProfit", model.OriginCalc, "", 999) // stale Calc cell
	usUnit := s.Ensure(unitKey("US"))
	usUnit.Stage(model.StageConsolidated)[cube.CellCoord{Account: "Sales", Origin: model.OriginForms}.Normalize()] = 12345
	deUnit := s.Ensure(unitKey("DE"))
	deUnit.Stage(model.StageElimination)[cube.CellCoord{Account: "Sales", Origin: model.OriginElim}.Normalize()] = 777

	res1, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process #1: %v", err)
	}

	cases := []cellCase{
		// GP per tuple: US {None}=1000, {DE}=200; DE {None}=400, {US}=-180.
		{"US GP local", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local"}, 1200},
		{"US GP local Calc IC=None", cube.POV{Entity: "US", Account: "GrossProfit", Stage: "Local", Origin: model.OriginCalc, IC: "None"}, 1000},
		{"US Sales consolidated (stale cleared)", cube.POV{Entity: "US", Account: "Sales"}, 1200},
		{"DE Elim (stale cleared)", cube.POV{Entity: "DE", Account: "Sales", Stage: "Elimination"}, 0},
		{"Global Sales", cube.POV{Entity: "Global", Account: "Sales"}, 1440},
		{"Global Elim Sales IC=DE", cube.POV{Entity: "Global", Account: "Sales", Stage: "Elimination", IC: "DE"}, -200},
	}
	checkCells(t, m, s, cases)

	res2, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process #2: %v", err)
	}
	if !reflect.DeepEqual(res1, res2) {
		t.Fatalf("Process not idempotent:\n#1 %+v\n#2 %+v", res1, res2)
	}
	checkCells(t, m, s, cases)
}

func TestProcessEmptySlice(t *testing.T) {
	m, _ := fixtureA(t)
	s := cube.NewStore() // no data at all
	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.UnitsProcessed != 0 || res.CellsWritten != 0 || len(res.Issues) != 0 {
		t.Errorf("got %+v, want zero counts and no issues", res)
	}
	if res.Issues == nil {
		t.Error("Issues must be non-nil (serializes as [])")
	}
}

func TestProcessErrors(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(t *testing.T, m *model.Metadata)
		cube    string
		scen    string
		month   string
		wantSub string
	}{
		{
			name: "unknown cube", cube: "Nope", scen: tScen, month: tMonth,
			wantSub: `cube "Nope" not found`,
		},
		{
			name: "quarter is not a month", cube: tCube, scen: tScen, month: "2025Q1",
			wantSub: "not a month",
		},
		{
			name: "year is not a month", cube: tCube, scen: tScen, month: "2025",
			wantSub: "not a month",
		},
		{
			name: "empty scenario", cube: tCube, scen: "", month: tMonth,
			wantSub: "scenario",
		},
		{
			name: "formula cycle", cube: tCube, scen: tScen, month: tMonth,
			mutate: func(t *testing.T, m *model.Metadata) {
				addAccount(t, m, "AcctA", model.AccountRevenue, false, "A#AcctB")
				addAccount(t, m, "AcctB", model.AccountRevenue, false, "A#AcctA")
			},
			wantSub: "cycle",
		},
		{
			name: "formula parse error", cube: tCube, scen: tScen, month: tMonth,
			mutate: func(t *testing.T, m *model.Metadata) {
				addAccount(t, m, "Bad", model.AccountRevenue, false, "1 +")
			},
			wantSub: `account "Bad"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, s := fixtureA(t)
			if tc.mutate != nil {
				tc.mutate(t, m)
			}
			res, err := consol.Process(m, s, tc.cube, tc.scen, tc.month)
			if err == nil {
				t.Fatalf("Process = %+v, want error containing %q", res, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantSub)
			}
		})
	}

	t.Run("nil metadata", func(t *testing.T) {
		if _, err := consol.Process(nil, cube.NewStore(), tCube, tScen, tMonth); err == nil {
			t.Error("want error for nil metadata")
		}
	})
	t.Run("nil store", func(t *testing.T) {
		m, _ := fixtureA(t)
		if _, err := consol.Process(m, nil, tCube, tScen, tMonth); err == nil {
			t.Error("want error for nil store")
		}
	})
}

// TestProcessDynamicCalcSkipped pins that DynamicCalc accounts are never
// materialized by Process (they evaluate at query time only).
func TestProcessDynamicCalcSkipped(t *testing.T) {
	m, s := fixtureA(t)
	addMember(t, m.Account(), &model.Member{
		Name: "GPMargin", AccountType: model.AccountNonFinancial,
		DynamicCalc: true, Formula: "A#Sales * 2",
	})

	if _, err := consol.Process(m, s, tCube, tScen, tMonth); err != nil {
		t.Fatalf("Process: %v", err)
	}
	// No stored cells for the dynamic account at any origin/stage.
	checkCells(t, m, s, []cellCase{
		{"GPMargin local Calc", cube.POV{Entity: "US", Account: "GPMargin", Stage: "Local", Origin: model.OriginCalc}, 0},
		{"GPMargin local", cube.POV{Entity: "US", Account: "GPMargin", Stage: "Local"}, 0},
	})
}

// TestProcessOtherSlicesUntouched ensures Process only rebuilds the
// requested (cube, scenario, month) slice.
func TestProcessOtherSlicesUntouched(t *testing.T) {
	m, s := fixtureA(t)
	// Data in a different month must not be processed.
	u := s.Ensure(cube.UnitKey{Cube: tCube, Entity: "US", Scenario: tScen, Time: "2025M2"})
	u.Input[cube.CellCoord{Account: "Sales", Origin: model.OriginImport}.Normalize()] = 50

	res, err := consol.Process(m, s, tCube, tScen, tMonth)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.UnitsProcessed != 3 {
		t.Errorf("UnitsProcessed = %d, want 3", res.UnitsProcessed)
	}
	m2 := s.Unit(cube.UnitKey{Cube: tCube, Entity: "US", Scenario: tScen, Time: "2025M2"})
	if len(m2.Stages) != 0 {
		t.Errorf("2025M2 unit gained stages: %v", m2.Stages)
	}
}
