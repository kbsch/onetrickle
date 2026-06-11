package stage

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
)

const eps = 1e-9

func almostEqual(a, b float64) bool { return math.Abs(a-b) <= eps }

// testMeta builds a small but realistic metadata fixture:
//
//	Entity:   Global → {US, DE}
//	Account:  Sales (Revenue, leaf), OpEx → {Salaries, Rent},
//	          GPMargin (DynamicCalc), GrossProfit (stored formula)
//	Scenario: Actual, Budget
//	Time:     2025 (months 2025M1..M12)
func testMeta(t *testing.T) *model.Metadata {
	t.Helper()
	m := model.NewMetadata()
	m.Cubes["Golf"] = &model.Cube{Name: "Golf", GroupCurrency: "USD"}

	ent := m.Dim(model.DimEntity)
	for _, mem := range []*model.Member{
		{Name: "Global", Currency: "USD"},
		{Name: "US", Parent: "Global", Currency: "USD"},
		{Name: "DE", Parent: "Global", Currency: "EUR"},
	} {
		if err := ent.AddMember(mem); err != nil {
			t.Fatalf("add entity %s: %v", mem.Name, err)
		}
	}

	acct := m.Dim(model.DimAccount)
	for _, mem := range []*model.Member{
		{Name: "Sales", AccountType: model.AccountRevenue, IsIC: true},
		{Name: "OpEx", AccountType: model.AccountExpense},
		{Name: "Salaries", Parent: "OpEx", AccountType: model.AccountExpense},
		{Name: "Rent", Parent: "OpEx", AccountType: model.AccountExpense},
		{Name: "GPMargin", AccountType: model.AccountNonFinancial, DynamicCalc: true, Formula: "A#Sales"},
		{Name: "GrossProfit", AccountType: model.AccountRevenue, Formula: "A#Sales - A#Rent"},
	} {
		if err := acct.AddMember(mem); err != nil {
			t.Fatalf("add account %s: %v", mem.Name, err)
		}
	}

	scen := m.Dim(model.DimScenario)
	for _, name := range []string{"Actual", "Budget"} {
		if err := scen.AddMember(&model.Member{Name: name}); err != nil {
			t.Fatalf("add scenario %s: %v", name, err)
		}
	}

	m.Dims[model.DimTime] = model.BuildTimeDim([]int{2025})
	return m
}

// baseProfile maps Entity,Account,Time from columns 0..2, amount from column
// 3, and fixes Scenario to Actual.
func baseProfile() *Profile {
	return &Profile{
		Name: "p1",
		Cube: "Golf",
		Columns: map[model.DimType]ColumnSpec{
			model.DimEntity:  {Col: 0},
			model.DimAccount: {Col: 1},
			model.DimTime:    {Col: 2},
			model.DimScenario: {
				Col:   -1,
				Fixed: "Actual",
			},
		},
		AmountCol: 3,
	}
}

func mustTransform(t *testing.T, p *Profile, data string) *TransformResult {
	t.Helper()
	res, err := Transform(p, []byte(data))
	if err != nil {
		t.Fatalf("Transform: %v", err)
	}
	return res
}

func TestTransformBasic(t *testing.T) {
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,100.5\nDE,Rent,2025M2,-7.25\n")
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(res.Rows))
	}
	tests := []struct {
		i            int
		line         int
		entity, acct string
		time         string
		amount       float64
		scen, flow   string
		ic, ud1      string
	}{
		{0, 1, "US", "Sales", "2025M1", 100.5, "Actual", "None", "None", "None"},
		{1, 2, "DE", "Rent", "2025M2", -7.25, "Actual", "None", "None", "None"},
	}
	for _, tc := range tests {
		r := res.Rows[tc.i]
		if r.Line != tc.line {
			t.Errorf("row %d Line = %d, want %d", tc.i, r.Line, tc.line)
		}
		if r.Entity != tc.entity || r.Account != tc.acct || r.Time != tc.time || r.Scenario != tc.scen {
			t.Errorf("row %d = %s/%s/%s/%s, want %s/%s/%s/%s",
				tc.i, r.Entity, r.Account, r.Time, r.Scenario, tc.entity, tc.acct, tc.time, tc.scen)
		}
		if !almostEqual(r.Amount, tc.amount) {
			t.Errorf("row %d Amount = %v, want %v", tc.i, r.Amount, tc.amount)
		}
		if r.Flow != tc.flow || r.IC != tc.ic || r.UD1 != tc.ud1 {
			t.Errorf("row %d defaults = flow %q ic %q ud1 %q, want %q/%q/%q",
				tc.i, r.Flow, r.IC, r.UD1, tc.flow, tc.ic, tc.ud1)
		}
		if len(r.Issues) != 0 {
			t.Errorf("row %d issues = %v, want none", tc.i, r.Issues)
		}
	}
}

func TestTransformHeaderSkip(t *testing.T) {
	p := baseProfile()
	p.HasHeader = true
	res := mustTransform(t, p, "entity,account,time,amount\nUS,Sales,2025M1,100\n")
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1 (header skipped)", len(res.Rows))
	}
	r := res.Rows[0]
	if r.Line != 2 {
		t.Errorf("Line = %d, want 2", r.Line)
	}
	if r.Entity != "US" || !almostEqual(r.Amount, 100) {
		t.Errorf("row = %s amount %v, want US amount 100", r.Entity, r.Amount)
	}
}

func TestTransformNoHeaderKeepsFirstRow(t *testing.T) {
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,100\n")
	if len(res.Rows) != 1 || res.Rows[0].Line != 1 {
		t.Fatalf("rows = %d (line %d), want 1 row at line 1", len(res.Rows), res.Rows[0].Line)
	}
}

func TestTransformFixedColumns(t *testing.T) {
	p := baseProfile()
	p.Columns[model.DimFlow] = ColumnSpec{Col: -1, Fixed: "None"}
	p.Columns[model.DimUD1] = ColumnSpec{Col: -1, Fixed: "Region1"}
	res := mustTransform(t, p, "US,Sales,2025M1,1\n")
	r := res.Rows[0]
	if r.Scenario != "Actual" {
		t.Errorf("Scenario = %q, want fixed %q", r.Scenario, "Actual")
	}
	if r.Flow != "None" || r.UD1 != "Region1" {
		t.Errorf("Flow/UD1 = %q/%q, want None/Region1", r.Flow, r.UD1)
	}
	if got := r.Raw[model.DimUD1]; got != "Region1" {
		t.Errorf("Raw[UD1] = %q, want Region1", got)
	}
}

func TestTransformCustomDelimiter(t *testing.T) {
	p := baseProfile()
	p.Delimiter = ";"
	res := mustTransform(t, p, "US;Sales;2025M1;42.5\n")
	r := res.Rows[0]
	if r.Entity != "US" || r.Account != "Sales" || !almostEqual(r.Amount, 42.5) {
		t.Errorf("row = %s/%s/%v, want US/Sales/42.5", r.Entity, r.Account, r.Amount)
	}
}

func TestTransformBadDelimiter(t *testing.T) {
	p := baseProfile()
	p.Delimiter = ";;"
	if _, err := Transform(p, []byte("a;;b\n")); err == nil {
		t.Fatal("Transform with 2-char delimiter: want error, got nil")
	}
}

func TestTransformExactBeatsPrefix(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{
		{Dim: model.DimAccount, Kind: KindPrefix, Src: "41*", Target: "Rent"},
		{Dim: model.DimAccount, Kind: KindExact, Src: "4100", Target: "Sales"},
	}
	res := mustTransform(t, p, "US,4100,2025M1,1\nUS,4150,2025M1,2\n")
	if got := res.Rows[0].Account; got != "Sales" {
		t.Errorf("exact: Account = %q, want Sales", got)
	}
	if got := res.Rows[1].Account; got != "Rent" {
		t.Errorf("prefix: Account = %q, want Rent", got)
	}
	if got := res.Rows[0].Raw[model.DimAccount]; got != "4100" {
		t.Errorf("Raw[Account] = %q, want 4100 (pre-mapping)", got)
	}
}

func TestTransformLongestPrefixWins(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{
		{Dim: model.DimAccount, Kind: KindPrefix, Src: "4*", Target: "OpEx"},
		{Dim: model.DimAccount, Kind: KindPrefix, Src: "415*", Target: "Salaries"},
		{Dim: model.DimAccount, Kind: KindPrefix, Src: "41*", Target: "Rent"},
	}
	res := mustTransform(t, p, "US,4159,2025M1,1\nUS,4190,2025M1,2\nUS,4900,2025M1,3\n")
	want := []string{"Salaries", "Rent", "OpEx"}
	for i, w := range want {
		if got := res.Rows[i].Account; got != w {
			t.Errorf("row %d Account = %q, want %q", i, got, w)
		}
	}
}

func TestTransformDefaultRule(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{
		{Dim: model.DimAccount, Kind: KindExact, Src: "4100", Target: "Sales"},
		{Dim: model.DimAccount, Kind: KindDefault, Src: "*", Target: "Rent"},
	}
	res := mustTransform(t, p, "US,9999,2025M1,1\nUS,4100,2025M1,2\n")
	if got := res.Rows[0].Account; got != "Rent" {
		t.Errorf("default: Account = %q, want Rent", got)
	}
	if got := res.Rows[1].Account; got != "Sales" {
		t.Errorf("exact over default: Account = %q, want Sales", got)
	}
}

func TestTransformIdentityFallback(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{
		// Rules on Entity must not leak into Account resolution.
		{Dim: model.DimEntity, Kind: KindExact, Src: "Sales", Target: "DE"},
	}
	res := mustTransform(t, p, "US,Sales,2025M1,1\n")
	r := res.Rows[0]
	if r.Account != "Sales" {
		t.Errorf("Account = %q, want identity Sales", r.Account)
	}
	if r.Entity != "US" {
		t.Errorf("Entity = %q, want identity US (entity rule src does not match)", r.Entity)
	}
}

func TestTransformRuleKindInference(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{
		{Dim: model.DimAccount, Src: "41*", Target: "Rent"},   // inferred prefix
		{Dim: model.DimAccount, Src: "4100", Target: "Sales"}, // inferred exact
		{Dim: model.DimAccount, Src: "*", Target: "OpEx"},     // inferred default
	}
	res := mustTransform(t, p, "US,4100,2025M1,1\nUS,4150,2025M1,2\nUS,zzz,2025M1,3\n")
	want := []string{"Sales", "Rent", "OpEx"}
	for i, w := range want {
		if got := res.Rows[i].Account; got != w {
			t.Errorf("row %d Account = %q, want %q", i, got, w)
		}
	}
}

func TestTransformUnknownRuleKind(t *testing.T) {
	p := baseProfile()
	p.Rules = []Rule{{Dim: model.DimAccount, Kind: "regex", Src: "4.*", Target: "Sales"}}
	if _, err := Transform(p, []byte("US,4100,2025M1,1\n")); err == nil {
		t.Fatal("Transform with unknown rule kind: want error, got nil")
	}
}

func TestTransformBadAmount(t *testing.T) {
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,abc\nUS,Sales,2025M2, 7 \n")
	if len(res.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (bad row retained)", len(res.Rows))
	}
	bad := res.Rows[0]
	if len(bad.Issues) != 1 || !strings.Contains(bad.Issues[0], `bad amount "abc"`) {
		t.Errorf("issues = %v, want one containing `bad amount \"abc\"`", bad.Issues)
	}
	if !strings.HasPrefix(bad.Issues[0], "line 1: ") {
		t.Errorf("issue %q should be prefixed with line number", bad.Issues[0])
	}
	good := res.Rows[1]
	if len(good.Issues) != 0 || !almostEqual(good.Amount, 7) {
		t.Errorf("trimmed amount row: issues=%v amount=%v, want none/7", good.Issues, good.Amount)
	}
}

// TestTransformNonFiniteAmount pins that NaN/Inf literals — which
// strconv.ParseFloat accepts — are flagged as bad amounts: a non-finite value
// in the store would make every JSON snapshot save fail forever.
func TestTransformNonFiniteAmount(t *testing.T) {
	for _, amt := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "Infinity"} {
		t.Run(amt, func(t *testing.T) {
			res := mustTransform(t, baseProfile(), "US,Sales,2025M1,"+amt+"\n")
			if len(res.Rows) != 1 {
				t.Fatalf("rows = %d, want 1", len(res.Rows))
			}
			r := res.Rows[0]
			if len(r.Issues) != 1 || !strings.Contains(r.Issues[0], "bad amount") {
				t.Errorf("issues = %v, want one bad-amount issue", r.Issues)
			}
			if r.Amount != 0 {
				t.Errorf("Amount = %v, want 0 (unset)", r.Amount)
			}
		})
	}
}

func TestTransformShortRow(t *testing.T) {
	res := mustTransform(t, baseProfile(), "US,Sales\nUS,Sales,2025M1,5\n")
	short := res.Rows[0]
	if len(short.Issues) == 0 {
		t.Fatal("short row: want issues for missing columns, got none")
	}
	joined := strings.Join(short.Issues, "; ")
	if !strings.Contains(joined, "missing column 2 for Time") {
		t.Errorf("issues = %v, want missing column 2 for Time", short.Issues)
	}
	if !strings.Contains(joined, "missing amount column 3") {
		t.Errorf("issues = %v, want missing amount column 3", short.Issues)
	}
	if len(res.Rows[1].Issues) != 0 {
		t.Errorf("full row should have no issues, got %v", res.Rows[1].Issues)
	}
}

func TestTransformMissingRequiredSpec(t *testing.T) {
	for _, dim := range []model.DimType{model.DimEntity, model.DimAccount, model.DimScenario, model.DimTime} {
		p := baseProfile()
		delete(p.Columns, dim)
		if _, err := Transform(p, []byte("US,Sales,2025M1,1\n")); err == nil {
			t.Errorf("missing %s spec: want error, got nil", dim)
		} else if !strings.Contains(err.Error(), string(dim)) {
			t.Errorf("missing %s spec: error %q should name the dimension", dim, err)
		}
	}
}

func TestTransformNegativeAmountCol(t *testing.T) {
	p := baseProfile()
	p.AmountCol = -1
	if _, err := Transform(p, []byte("US,Sales,2025M1,1\n")); err == nil {
		t.Fatal("negative AmountCol: want error, got nil")
	}
}

func TestTransformNilProfile(t *testing.T) {
	if _, err := Transform(nil, []byte("x\n")); err == nil {
		t.Fatal("nil profile: want error, got nil")
	}
}

func TestTransformEmptyInput(t *testing.T) {
	res := mustTransform(t, baseProfile(), "")
	if len(res.Rows) != 0 || len(res.Issues) != 0 {
		t.Fatalf("empty input: rows=%d issues=%v, want 0/none", len(res.Rows), res.Issues)
	}
}

func TestValidateCleanRows(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,100\nDE,Rent,2025M12,5\n")
	Validate(meta, res)
	for i, r := range res.Rows {
		if len(r.Issues) != 0 {
			t.Errorf("row %d issues = %v, want none", i, r.Issues)
		}
	}
	if len(res.Issues) != 0 {
		t.Errorf("global issues = %v, want none", res.Issues)
	}
}

func TestValidateUnknownMember(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,4100,2025M1,1\nMars,Sales,2025M1,2\n")
	Validate(meta, res)
	if got, want := res.Rows[0].Issues, `line 1: unknown Account "4100"`; len(got) != 1 || got[0] != want {
		t.Errorf("row 0 issues = %v, want exactly [%q]", got, want)
	}
	if got, want := res.Rows[1].Issues, `line 2: unknown Entity "Mars"`; len(got) != 1 || got[0] != want {
		t.Errorf("row 1 issues = %v, want exactly [%q]", got, want)
	}
}

func TestValidateGlobalSummary(t *testing.T) {
	meta := testMeta(t)
	// Two rows with the same unknown account, one with an unknown entity.
	res := mustTransform(t, baseProfile(), "US,4100,2025M1,1\nDE,4100,2025M2,2\nMars,Sales,2025M1,3\n")
	Validate(meta, res)
	want := []string{
		`unknown Account "4100" (2 rows)`,
		`unknown Entity "Mars" (1 row)`,
	}
	if len(res.Issues) != len(want) {
		t.Fatalf("global issues = %v, want %v", res.Issues, want)
	}
	for i, w := range want {
		if res.Issues[i] != w {
			t.Errorf("global issue %d = %q, want %q", i, res.Issues[i], w)
		}
	}
}

func TestValidateNonLeafRejection(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "Global,Sales,2025M1,1\nUS,OpEx,2025M1,2\n")
	Validate(meta, res)
	if got := res.Rows[0].Issues; len(got) != 1 || !strings.Contains(got[0], `Entity "Global" is not a leaf`) {
		t.Errorf("row 0 issues = %v, want non-leaf Entity", got)
	}
	if got := res.Rows[1].Issues; len(got) != 1 || !strings.Contains(got[0], `Account "OpEx" is not a leaf`) {
		t.Errorf("row 1 issues = %v, want non-leaf Account", got)
	}
}

func TestValidateNonMonthTime(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,Sales,2025Q1,1\nUS,Sales,2025,2\nUS,Sales,2030M1,3\n")
	Validate(meta, res)
	if got := res.Rows[0].Issues; len(got) != 1 || !strings.Contains(got[0], `Time "2025Q1" is not a month`) {
		t.Errorf("quarter: issues = %v, want not-a-month", got)
	}
	if got := res.Rows[1].Issues; len(got) != 1 || !strings.Contains(got[0], `Time "2025" is not a month`) {
		t.Errorf("year: issues = %v, want not-a-month", got)
	}
	if got := res.Rows[2].Issues; len(got) != 1 || !strings.Contains(got[0], `unknown Time "2030M1"`) {
		t.Errorf("missing month: issues = %v, want unknown Time", got)
	}
}

func TestValidateIC(t *testing.T) {
	meta := testMeta(t)
	p := baseProfile()
	p.Columns[DimIC] = ColumnSpec{Col: 4}
	res := mustTransform(t, p, "US,Sales,2025M1,1,DE\nUS,Sales,2025M1,2,None\nUS,Sales,2025M1,3,Mars\n")
	Validate(meta, res)
	if got := res.Rows[0].Issues; len(got) != 0 {
		t.Errorf("IC=DE (entity): issues = %v, want none", got)
	}
	if got := res.Rows[1].Issues; len(got) != 0 {
		t.Errorf("IC=None: issues = %v, want none", got)
	}
	if got := res.Rows[2].Issues; len(got) != 1 || !strings.Contains(got[0], `invalid IC partner "Mars"`) {
		t.Errorf("IC=Mars: issues = %v, want invalid IC partner", got)
	}
}

func TestValidateCalcAccountRejection(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,GPMargin,2025M1,1\nUS,GrossProfit,2025M1,2\n")
	Validate(meta, res)
	if got := res.Rows[0].Issues; len(got) != 1 || !strings.Contains(got[0], `Account "GPMargin" is a dynamic-calc account`) {
		t.Errorf("dynamic-calc: issues = %v, want rejection", got)
	}
	if got := res.Rows[1].Issues; len(got) != 1 || !strings.Contains(got[0], `Account "GrossProfit" has a formula`) {
		t.Errorf("formula: issues = %v, want rejection", got)
	}
}

func TestLoadPlanGrouping(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(),
		"US,Sales,2025M1,100\nUS,Rent,2025M1,30\nDE,Sales,2025M1,40\nUS,Sales,2025M2,110\n")
	Validate(meta, res)
	plan := LoadPlan(res)
	if len(plan) != 3 {
		t.Fatalf("plan units = %d, want 3", len(plan))
	}
	ukUS1 := cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"}
	ukDE1 := cube.UnitKey{Cube: "Golf", Entity: "DE", Scenario: "Actual", Time: "2025M1"}
	ukUS2 := cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M2"}
	if got := len(plan[ukUS1]); got != 2 {
		t.Errorf("US/2025M1 writes = %d, want 2", got)
	}
	if got := len(plan[ukDE1]); got != 1 {
		t.Errorf("DE/2025M1 writes = %d, want 1", got)
	}
	if got := len(plan[ukUS2]); got != 1 {
		t.Errorf("US/2025M2 writes = %d, want 1", got)
	}
	w := plan[ukDE1][0]
	wantCoord := cube.CellCoord{
		Account: "Sales", Flow: "None", Origin: model.OriginImport,
		IC: "None", UD1: "None", UD2: "None", UD3: "None", UD4: "None",
	}
	if w.Coord != wantCoord {
		t.Errorf("DE coord = %+v, want %+v", w.Coord, wantCoord)
	}
	if w.Unit != ukDE1 {
		t.Errorf("write Unit = %+v, want %+v", w.Unit, ukDE1)
	}
	if !almostEqual(w.Value, 40) {
		t.Errorf("DE value = %v, want 40", w.Value)
	}
}

func TestLoadPlanDuplicateCoordSumming(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,100\nUS,Sales,2025M1,25.5\nUS,Rent,2025M1,3\n")
	Validate(meta, res)
	plan := LoadPlan(res)
	uk := cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"}
	writes := plan[uk]
	if len(writes) != 2 {
		t.Fatalf("writes = %d, want 2 (duplicates summed)", len(writes))
	}
	byAcct := map[string]float64{}
	for _, w := range writes {
		byAcct[w.Coord.Account] = w.Value
	}
	if !almostEqual(byAcct["Sales"], 125.5) {
		t.Errorf("Sales = %v, want 125.5", byAcct["Sales"])
	}
	if !almostEqual(byAcct["Rent"], 3) {
		t.Errorf("Rent = %v, want 3", byAcct["Rent"])
	}
}

func TestLoadPlanSkipsFlaggedRows(t *testing.T) {
	meta := testMeta(t)
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,100\nUS,Bogus,2025M1,50\nUS,Sales,2025M1,abc\n")
	Validate(meta, res)
	plan := LoadPlan(res)
	uk := cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"}
	if len(plan) != 1 {
		t.Fatalf("plan units = %d, want 1", len(plan))
	}
	writes := plan[uk]
	if len(writes) != 1 || !almostEqual(writes[0].Value, 100) {
		t.Fatalf("writes = %+v, want single Sales=100", writes)
	}
}

func TestLoadPlanEmptyAndNil(t *testing.T) {
	if plan := LoadPlan(nil); len(plan) != 0 {
		t.Errorf("LoadPlan(nil) = %v, want empty", plan)
	}
	if plan := LoadPlan(&TransformResult{}); len(plan) != 0 {
		t.Errorf("LoadPlan(empty) = %v, want empty", plan)
	}
}

func TestProfileJSONTags(t *testing.T) {
	p := &Profile{
		Name:      "p1",
		Cube:      "Golf",
		HasHeader: true,
		Delimiter: ";",
		Columns: map[model.DimType]ColumnSpec{
			model.DimEntity: {Col: -1, Fixed: "US"},
		},
		AmountCol: 3,
		Rules:     []Rule{{Dim: model.DimAccount, Kind: KindPrefix, Src: "41*", Target: "Sales"}},
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{
		`"name":"p1"`, `"cube":"Golf"`, `"hasHeader":true`, `"delimiter":";"`,
		`"amountCol":3`, `"col":-1`, `"fixed":"US"`,
		`"dim":"Account"`, `"kind":"prefix"`, `"src":"41*"`, `"target":"Sales"`,
		`"columns"`, `"rules"`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("profile JSON missing %s in %s", key, s)
		}
	}
	var back Profile
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Name != p.Name || back.Cube != p.Cube || !back.HasHeader ||
		back.Delimiter != p.Delimiter || back.AmountCol != p.AmountCol {
		t.Errorf("round trip = %+v, want %+v", back, p)
	}
	if cs := back.Columns[model.DimEntity]; cs.Col != -1 || cs.Fixed != "US" {
		t.Errorf("round trip column = %+v, want {-1 US}", cs)
	}
	if len(back.Rules) != 1 || back.Rules[0] != p.Rules[0] {
		t.Errorf("round trip rules = %+v, want %+v", back.Rules, p.Rules)
	}
}

func TestMappedRowJSONTags(t *testing.T) {
	res := mustTransform(t, baseProfile(), "US,Sales,2025M1,abc\n")
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, key := range []string{
		`"rows"`, `"issues"`, `"line":1`, `"entity":"US"`, `"account":"Sales"`,
		`"scenario":"Actual"`, `"time":"2025M1"`, `"flow":"None"`, `"ic":"None"`,
		`"ud1":"None"`, `"ud2":"None"`, `"ud3":"None"`, `"ud4":"None"`,
		`"amount":0`, `"raw"`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("result JSON missing %s in %s", key, s)
		}
	}
}

func TestTransformQuotedFieldsAndSpaces(t *testing.T) {
	// Quoted member names containing the delimiter, plus padding spaces
	// around plain values, must both resolve cleanly.
	res := mustTransform(t, baseProfile(), "\"US\",\"Sales\",2025M1, 12.5\n US , Sales ,2025M2,1\n")
	if r := res.Rows[0]; r.Entity != "US" || !almostEqual(r.Amount, 12.5) {
		t.Errorf("quoted row = %s/%v, want US/12.5", r.Entity, r.Amount)
	}
	if r := res.Rows[1]; r.Entity != "US" || r.Account != "Sales" {
		t.Errorf("padded row = %q/%q, want US/Sales (trimmed)", r.Entity, r.Account)
	}
}

func TestLoadPlanDeterministicOrder(t *testing.T) {
	meta := testMeta(t)
	data := "US,Rent,2025M1,1\nUS,Sales,2025M1,2\nUS,Salaries,2025M1,3\n"
	res := mustTransform(t, baseProfile(), data)
	Validate(meta, res)
	uk := cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"}
	first := LoadPlan(res)[uk]
	for range 10 {
		again := LoadPlan(res)[uk]
		for i := range first {
			if first[i] != again[i] {
				t.Fatalf("non-deterministic plan order: %+v vs %+v", first, again)
			}
		}
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Coord.Key() >= first[i].Coord.Key() {
			t.Fatalf("writes not sorted by coord key: %+v", first)
		}
	}
}
