// Package seed builds the GolfTrickle demo dataset described in SPEC §14:
// metadata (cube, entity and account hierarchies, scenarios, time, FX rates),
// Origin=Import input data for both scenarios across every month of 2024,
// 2025 and 2026, and a CSV import profile with transformation rules.
//
// Everything is a pure deterministic function of entity/year/month/scenario
// indices — no randomness, no clock — so repeated Build calls produce
// byte-identical JSON. The seed does NOT run consolidation; the CLI is
// expected to run consol.Process for the seeded slices (see Slices) after
// seeding.
package seed

import (
	"fmt"
	"math"
	"strings"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/stage"
)

// Names of the demo objects, exported for the seed CLI / server wiring.
const (
	CubeName    = "GolfTrickle"
	ProfileName = "GolfTrickle CSV"

	ScenarioActual = "Actual"
	ScenarioBudget = "Budget"
)

// Deterministic data-shape parameters.
const (
	budgetFactor = 1.05 // Budget amounts = Actual-like base × 1.05

	// Intercompany pair: US Operations sells to Germany every seeded month.
	icSalesBase   = 200.0 // USD, month 1 of the 2025 baseline year
	icSalesGrowth = 10.0  // USD per month
	icEURRate     = 1.09  // matches the EUR Average base rate so the
	// translated USD values roughly offset
)

// dataYears are the years seeded with input data and FX rates. 2025 is the
// baseline; 2024/2026 amounts and rates are scaled/shifted off it (see
// yearFactor and rateYearDrift) so the three years show distinct numbers.
var dataYears = []int{2024, 2025, 2026}

// yearFactor scales every amount by data year: 2024 ≈ 10% lighter, 2026 ≈ 10%
// heavier than the 2025 baseline (factor 1.0). Holding 2025 at exactly 1.0
// keeps the 2025 figures — including the documented gold consolidation
// numbers — byte-identical to a single-year seed.
func yearFactor(y int) float64 {
	switch y {
	case 2024:
		return 0.90
	case 2026:
		return 1.10
	default:
		return 1.0
	}
}

// rateYearDrift shifts FX rates by data year (2025 unchanged) so rates differ
// across years as well as across months.
func rateYearDrift(y int) float64 {
	switch y {
	case 2024:
		return -0.02
	case 2026:
		return 0.02
	default:
		return 0
	}
}

// Slices returns every (scenario, month) data slice the seed populates, in a
// deterministic order. The CLI consolidates and workflow-processes each one.
func Slices() []struct{ Scenario, Time string } {
	var out []struct{ Scenario, Time string }
	for _, sc := range []string{ScenarioActual, ScenarioBudget} {
		for _, y := range dataYears {
			for m := 1; m <= 12; m++ {
				out = append(out, struct{ Scenario, Time string }{sc, model.TimeName(y, m)})
			}
		}
	}
	return out
}

// Build constructs the GolfTrickle demo: metadata, cell store and import
// profiles. It is deterministic and never mutates package state.
func Build() (*model.Metadata, *cube.Store, map[string]*stage.Profile, error) {
	meta, err := buildMetadata()
	if err != nil {
		return nil, nil, nil, err
	}
	if probs := meta.Validate(); len(probs) > 0 {
		return nil, nil, nil, fmt.Errorf("seed: metadata validation failed: %s", strings.Join(probs, "; "))
	}
	return meta, buildStore(), buildProfiles(), nil
}

// buildMetadata assembles cube, dimensions and FX rates.
func buildMetadata() (*model.Metadata, error) {
	meta := model.NewMetadata()
	meta.Dims[model.DimTime] = model.BuildTimeDim([]int{2024, 2025, 2026})
	meta.Cubes[CubeName] = &model.Cube{
		Name:          CubeName,
		Description:   "GolfTrickle Inc demo",
		GroupCurrency: "USD",
	}
	for _, m := range entityMembers() {
		if err := meta.Entity().AddMember(m); err != nil {
			return nil, fmt.Errorf("seed: add entity %q: %w", m.Name, err)
		}
	}
	for _, m := range accountMembers() {
		if err := meta.Account().AddMember(m); err != nil {
			return nil, fmt.Errorf("seed: add account %q: %w", m.Name, err)
		}
	}
	for _, s := range []string{ScenarioActual, ScenarioBudget} {
		if err := meta.Dim(model.DimScenario).AddMember(&model.Member{Name: s}); err != nil {
			return nil, fmt.Errorf("seed: add scenario %q: %w", s, err)
		}
	}
	if err := addRates(meta.Rates); err != nil {
		return nil, err
	}
	return meta, nil
}

// entityMembers returns the entity tree in insertion (parent-first) order.
// GolfTrickle Inc(USD) → North America(USD) → {US Operations(USD),
// Canada(CAD)}; → Europe(EUR) → {Germany(EUR), France(EUR, 80%)}.
func entityMembers() []*model.Member {
	return []*model.Member{
		{Name: "GolfTrickle Inc", Currency: "USD"},
		{Name: "North America", Parent: "GolfTrickle Inc", Currency: "USD"},
		{Name: "US Operations", Parent: "North America", Currency: "USD"},
		{Name: "Canada", Parent: "North America", Currency: "CAD"},
		{Name: "Europe", Parent: "GolfTrickle Inc", Currency: "EUR"},
		{Name: "Germany", Parent: "Europe", Currency: "EUR"},
		{Name: "France", Parent: "Europe", Currency: "EUR", OwnershipPct: 80},
	}
}

// accountMembers returns the account tree in insertion (parent-first) order.
// Sales and COGS are deliberately ROOT members — GrossProfit's stored-calc
// formula reads them; they do not roll up into NetIncome directly.
func accountMembers() []*model.Member {
	return []*model.Member{
		{Name: "NetIncome", AccountType: model.AccountRevenue},
		{Name: "GrossProfit", Parent: "NetIncome", Weight: 1,
			AccountType: model.AccountRevenue, Formula: "A#Sales - A#COGS"},
		{Name: "OpEx", Parent: "NetIncome", Weight: -1, AccountType: model.AccountExpense},
		{Name: "Salaries", Parent: "OpEx", AccountType: model.AccountExpense},
		{Name: "Marketing", Parent: "OpEx", AccountType: model.AccountExpense},
		{Name: "Rent", Parent: "OpEx", AccountType: model.AccountExpense},
		{Name: "Sales", AccountType: model.AccountRevenue, IsIC: true},
		{Name: "COGS", AccountType: model.AccountExpense, IsIC: true},
		{Name: "BalanceSheet", AccountType: model.AccountAsset},
		{Name: "Cash", Parent: "BalanceSheet", AccountType: model.AccountAsset},
		{Name: "Receivables", Parent: "BalanceSheet", AccountType: model.AccountAsset},
		{Name: "Payables", Parent: "BalanceSheet", AccountType: model.AccountLiability},
		{Name: "GPMargin", AccountType: model.AccountNonFinancial, DynamicCalc: true,
			Formula: "IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)"},
	}
}

// addRates fills the rate table for every month of every dataYear in both
// scenarios: CAD 0.74 avg / 0.73 close, EUR 1.09 avg / 1.08 close at the 2025
// baseline, drifting +0.001 per month (so months differ) and shifted per year
// by rateYearDrift (so years differ).
func addRates(rt model.RateTable) error {
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
		for _, y := range dataYears {
			for m := 1; m <= 12; m++ {
				t := model.TimeName(y, m)
				drift := rateYearDrift(y) + 0.001*float64(m-1)
				for _, b := range bases {
					if err := rt.Set(sc, t, b.currency, b.typ, b.base+drift); err != nil {
						return fmt.Errorf("seed: set rate %s/%s %s %s: %w", sc, t, b.currency, b.typ, err)
					}
				}
			}
		}
	}
	return nil
}

// entityBase carries the deterministic amount bases of one leaf entity
// (local-currency values at 2025M1, Actual).
type entityBase struct {
	name        string
	sales       float64 // Sales at month 1
	growth      float64 // Sales increase per month
	cogsPct     float64 // COGS as a fraction of Sales (0.55–0.65)
	salaries    float64
	marketing   float64
	rent        float64
	cash        float64
	receivables float64
	payables    float64
}

// entityBases returns the per-entity amount bases (a fresh slice each call —
// no shared mutable package state).
func entityBases() []entityBase {
	return []entityBase{
		{name: "US Operations", sales: 1000, growth: 25, cogsPct: 0.58,
			salaries: 220, marketing: 80, rent: 45, cash: 5000, receivables: 1200, payables: 800},
		{name: "Canada", sales: 600, growth: 15, cogsPct: 0.62,
			salaries: 140, marketing: 40, rent: 30, cash: 2500, receivables: 700, payables: 450},
		{name: "Germany", sales: 850, growth: 20, cogsPct: 0.60,
			salaries: 180, marketing: 60, rent: 38, cash: 3200, receivables: 900, payables: 600},
		{name: "France", sales: 500, growth: 12, cogsPct: 0.56,
			salaries: 110, marketing: 35, rent: 25, cash: 1800, receivables: 500, payables: 350},
	}
}

// cell pairs an account with a value for one entity-month write.
type cell struct {
	account string
	value   float64
}

// baseCells returns the (account, value) pairs of one entity-month. factor is
// 1.0 for Actual and budgetFactor for Budget; m is the month (1-based). All
// values are rounded to 2 decimals.
func baseCells(b entityBase, m int, factor float64) []cell {
	k := float64(m - 1)
	sales := round2((b.sales + b.growth*k) * factor)
	return []cell{
		{"Sales", sales},
		{"COGS", round2(sales * b.cogsPct)},
		{"Salaries", round2((b.salaries + 3*k) * factor)},
		{"Marketing", round2((b.marketing + 2*k) * factor)},
		{"Rent", round2(b.rent * factor)},
		{"Cash", round2((b.cash + 100*k) * factor)},
		{"Receivables", round2((b.receivables + 20*k) * factor)},
		{"Payables", round2((b.payables + 15*k) * factor)},
	}
}

// icSales returns the baseline US Operations → Germany intercompany sale for
// month m (USD, 2025 scale); callers scale it by yearFactor. The Germany COGS
// counterpart is this amount divided by the EUR base rate (local EUR).
func icSales(m int) float64 { return icSalesBase + icSalesGrowth*float64(m-1) }

// buildStore writes the Origin=Import input data for every leaf entity across
// both scenarios and every month of every dataYear (2024/2025/2026), plus the
// US Operations → Germany IC pair each month. Amounts are scaled by scenario
// (Budget = ×budgetFactor) and by year (yearFactor).
func buildStore() *cube.Store {
	store := cube.NewStore()
	scenarios := []struct {
		name   string
		factor float64
	}{
		{ScenarioActual, 1.0},
		{ScenarioBudget, budgetFactor},
	}
	for _, sc := range scenarios {
		for _, y := range dataYears {
			yf := yearFactor(y)
			for m := 1; m <= 12; m++ {
				t := model.TimeName(y, m)
				for _, b := range entityBases() {
					uk := cube.UnitKey{Cube: CubeName, Entity: b.name, Scenario: sc.name, Time: t}
					u := store.Ensure(uk)
					for _, c := range baseCells(b, m, sc.factor*yf) {
						u.Input[importCoord(c.account, "")] = c.value
					}
				}
				// Intercompany pair: US Operations sells to Germany.
				usd := round2(icSales(m) * yf)
				us := cube.UnitKey{Cube: CubeName, Entity: "US Operations", Scenario: sc.name, Time: t}
				store.Ensure(us).Input[importCoord("Sales", "Germany")] = usd
				de := cube.UnitKey{Cube: CubeName, Entity: "Germany", Scenario: sc.name, Time: t}
				store.Ensure(de).Input[importCoord("COGS", "US Operations")] = round2(usd / icEURRate)
			}
		}
	}
	return store
}

// importCoord builds a normalized Origin=Import coordinate (ic "" → None).
func importCoord(account, ic string) cube.CellCoord {
	return cube.CellCoord{Account: account, Origin: model.OriginImport, IC: ic}.Normalize()
}

// buildProfiles returns the demo import profiles. "GolfTrickle CSV" maps a
// 4-column file (entity, account, time, amount) onto the GolfTrickle cube
// with Scenario fixed to Actual; Flow/IC/UD1..4 are omitted and default to
// None during Transform. Rules: account code 4100 → Sales, codes 5* → COGS,
// and every entity defaults to US Operations.
func buildProfiles() map[string]*stage.Profile {
	return map[string]*stage.Profile{
		ProfileName: {
			Name:      ProfileName,
			Cube:      CubeName,
			HasHeader: true,
			Delimiter: ",",
			Columns: map[model.DimType]stage.ColumnSpec{
				model.DimEntity:   {Col: 0},
				model.DimAccount:  {Col: 1},
				model.DimTime:     {Col: 2},
				model.DimScenario: {Col: -1, Fixed: ScenarioActual},
			},
			AmountCol: 3,
			Rules: []stage.Rule{
				{Dim: model.DimAccount, Kind: stage.KindExact, Src: "4100", Target: "Sales"},
				{Dim: model.DimAccount, Kind: stage.KindPrefix, Src: "5*", Target: "COGS"},
				{Dim: model.DimEntity, Kind: stage.KindDefault, Src: "*", Target: "US Operations"},
			},
		},
	}
}

// SampleCSV returns a small importable CSV matching the "GolfTrickle CSV"
// profile: header + 6 rows of raw account codes (4100 → Sales, 5xxx → COGS)
// for 2025M7. The profile's default entity rule maps every entity value to
// US Operations, so the file loads cleanly into one data unit.
func SampleCSV() []byte {
	return []byte(`entity,account,time,amount
US Operations,4100,2025M7,1500
US Operations,5000,2025M7,870
Canada,4100,2025M7,720
Canada,5100,2025M7,445
Germany,4100,2025M7,980
France,5200,2025M7,300
`)
}

// round2 rounds to 2 decimal places (half away from zero).
func round2(v float64) float64 { return math.Round(v*100) / 100 }
