package store

import (
	"bytes"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/stage"
	"onetrickle/internal/workflow"
)

const eps = 1e-9

func approx(t *testing.T, got, want float64, msg string) {
	t.Helper()
	if math.Abs(got-want) > eps {
		t.Errorf("%s: got %v, want %v", msg, got, want)
	}
}

func mustAdd(t *testing.T, d *model.Dimension, m *model.Member) {
	t.Helper()
	if err := d.AddMember(m); err != nil {
		t.Fatalf("add member %q to %s: %v", m.Name, d.Type, err)
	}
}

// coord builds a normalized cell coordinate with only account/origin/ic set.
func coord(account, origin, ic string) cube.CellCoord {
	return cube.CellCoord{Account: account, Origin: origin, IC: ic}.Normalize()
}

// fixedAt is the deterministic timestamp used for workflow history (wall-clock
// UTC, so it survives a JSON round trip exactly).
var fixedAt = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

// wfKey is the data unit the workflow fixture tracks.
var wfKey = workflow.Key{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"}

// populatedState builds a state exercising every persisted structure:
// metadata with account properties and entity ownership, FX rates, units with
// Input and materialized stages (including Elim-origin cells), an import
// profile with rules, and workflow entries with history.
func populatedState(t *testing.T) *AppState {
	t.Helper()
	s := NewAppState()

	s.Meta.Cubes["Golf"] = &model.Cube{Name: "Golf", GroupCurrency: "USD"}

	ent := s.Meta.Entity()
	mustAdd(t, ent, &model.Member{Name: "Global", Currency: "USD"})
	mustAdd(t, ent, &model.Member{Name: "US", Parent: "Global", Currency: "USD"})
	mustAdd(t, ent, &model.Member{Name: "DE", Parent: "Global", Currency: "EUR", OwnershipPct: 80})

	acct := s.Meta.Account()
	mustAdd(t, acct, &model.Member{Name: "NetIncome", AccountType: model.AccountRevenue})
	mustAdd(t, acct, &model.Member{Name: "Sales", Parent: "NetIncome", AccountType: model.AccountRevenue, IsIC: true})
	mustAdd(t, acct, &model.Member{Name: "COGS", Parent: "NetIncome", Weight: -1, AccountType: model.AccountExpense, IsIC: true})
	mustAdd(t, acct, &model.Member{Name: "GrossProfit", Parent: "NetIncome", AccountType: model.AccountRevenue, Formula: "A#Sales - A#COGS"})
	mustAdd(t, acct, &model.Member{Name: "GPMargin", AccountType: model.AccountNonFinancial, DynamicCalc: true,
		Formula: "IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)"})
	mustAdd(t, acct, &model.Member{Name: "Cash", AccountType: model.AccountAsset})

	mustAdd(t, s.Meta.Dim(model.DimScenario), &model.Member{Name: "Actual"})
	model.AddTimeYear(s.Meta.Dim(model.DimTime), 2025)

	for _, r := range []struct {
		typ  model.RateType
		rate float64
	}{{model.RateAverage, 1.10}, {model.RateClosing, 1.08}} {
		if err := s.Meta.Rates.Set("Actual", "2025M1", "EUR", r.typ, r.rate); err != nil {
			t.Fatalf("set rate: %v", err)
		}
	}

	us := s.Cells.Ensure(cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"})
	us.Input[coord("Sales", model.OriginImport, "")] = 1000
	us.Input[coord("Sales", model.OriginImport, "DE")] = 200
	us.Input[coord("Cash", model.OriginForms, "")] = 500
	us.Stage(model.StageTranslated)[coord("Sales", model.OriginImport, "")] = 1000
	us.Stage(model.StageTranslated)[coord("Sales", model.OriginImport, "DE")] = 200

	de := s.Cells.Ensure(cube.UnitKey{Cube: "Golf", Entity: "DE", Scenario: "Actual", Time: "2025M1"})
	de.Input[coord("COGS", model.OriginImport, "US")] = 180
	de.Stage(model.StageTranslated)[coord("COGS", model.OriginImport, "US")] = 198

	g := s.Cells.Ensure(cube.UnitKey{Cube: "Golf", Entity: "Global", Scenario: "Actual", Time: "2025M1"})
	g.Stage(model.StageElimination)[coord("Sales", model.OriginElim, "DE")] = -200
	g.Stage(model.StageElimination)[coord("COGS", model.OriginElim, "US")] = -198
	g.Stage(model.StageConsolidated)[coord("Sales", model.OriginImport, "")] = 1440

	s.Profiles["GL"] = &stage.Profile{
		Name:      "GL",
		Cube:      "Golf",
		HasHeader: true,
		Delimiter: ";",
		Columns: map[model.DimType]stage.ColumnSpec{
			model.DimEntity:   {Col: 0},
			model.DimAccount:  {Col: 1},
			model.DimScenario: {Col: -1, Fixed: "Actual"},
			model.DimTime:     {Col: 2},
			stage.DimIC:       {Col: -1, Fixed: "None"},
		},
		AmountCol: 3,
		Rules: []stage.Rule{
			{Dim: model.DimAccount, Kind: stage.KindExact, Src: "4000", Target: "Sales"},
			{Dim: model.DimAccount, Kind: stage.KindPrefix, Src: "41*", Target: "COGS"},
			{Dim: model.DimEntity, Kind: stage.KindDefault, Src: "*", Target: "US"},
		},
	}

	for i, action := range []string{workflow.ActionImport, workflow.ActionValidate} {
		at := fixedAt.Add(time.Duration(i) * time.Minute)
		if _, err := s.Workflow.Apply(wfKey, action, "kb", at); err != nil {
			t.Fatalf("workflow apply %q: %v", action, err)
		}
	}
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// Nested non-existent directory: Save must MkdirAll the parent.
	path := filepath.Join(dir, "data", "nested", "onetrickle.json")

	orig := populatedState(t)
	if err := Save(path, orig); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file %s.tmp still exists after Save (err=%v)", path, err)
	}

	// Snapshot wire shape: top-level keys and version.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("snapshot is not a JSON object: %v", err)
	}
	for _, key := range []string{"version", "metadata", "units", "profiles", "workflow"} {
		if _, ok := top[key]; !ok {
			t.Errorf("snapshot missing top-level key %q", key)
		}
	}
	if string(top["version"]) != "1" {
		t.Errorf("snapshot version = %s, want 1", top["version"])
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Metadata leaves.
	if c, err := got.Meta.CubeOf("Golf"); err != nil || c.GroupCurrency != "USD" {
		t.Errorf("cube Golf = %+v, %v; want GroupCurrency USD", c, err)
	}
	de := got.Meta.Entity().Get("DE")
	if de == nil {
		t.Fatal("entity DE missing after reload")
	}
	if de.Currency != "EUR" || de.Parent != "Global" {
		t.Errorf("DE = %+v; want Currency EUR, Parent Global", de)
	}
	approx(t, de.OwnershipPct, 80, "DE.OwnershipPct")
	cogs := got.Meta.Account().Get("COGS")
	if cogs == nil {
		t.Fatal("account COGS missing after reload")
	}
	if cogs.AccountType != model.AccountExpense || !cogs.IsIC {
		t.Errorf("COGS = %+v; want Expense, IsIC", cogs)
	}
	approx(t, cogs.Weight, -1, "COGS.Weight")
	if gp := got.Meta.Account().Get("GrossProfit"); gp == nil || gp.Formula != "A#Sales - A#COGS" {
		t.Errorf("GrossProfit = %+v; want Formula %q", gp, "A#Sales - A#COGS")
	}
	if gm := got.Meta.Account().Get("GPMargin"); gm == nil || !gm.DynamicCalc || gm.Formula == "" {
		t.Errorf("GPMargin = %+v; want DynamicCalc with formula", gm)
	}
	if rate, ok := got.Meta.Rates.Get("Actual", "2025M1", "EUR", model.RateAverage); !ok {
		t.Error("EUR Average rate missing after reload")
	} else {
		approx(t, rate, 1.10, "EUR Average rate")
	}
	if !got.Meta.Dim(model.DimTime).Has("2025M7") {
		t.Error("Time member 2025M7 missing after reload")
	}

	// Cell leaves: Input and materialized stages, including Elim origin.
	us := got.Cells.Unit(cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"})
	if us == nil {
		t.Fatal("US unit missing after reload")
	}
	approx(t, us.Input[coord("Sales", model.OriginImport, "")], 1000, "US Sales input")
	approx(t, us.Input[coord("Sales", model.OriginImport, "DE")], 200, "US Sales IC=DE input")
	approx(t, us.Input[coord("Cash", model.OriginForms, "")], 500, "US Cash Forms input")
	approx(t, us.Stages[model.StageTranslated][coord("Sales", model.OriginImport, "DE")], 200, "US translated Sales IC=DE")
	g := got.Cells.Unit(cube.UnitKey{Cube: "Golf", Entity: "Global", Scenario: "Actual", Time: "2025M1"})
	if g == nil {
		t.Fatal("Global unit missing after reload")
	}
	approx(t, g.Stages[model.StageElimination][coord("Sales", model.OriginElim, "DE")], -200, "Global elim Sales")
	approx(t, g.Stages[model.StageElimination][coord("COGS", model.OriginElim, "US")], -198, "Global elim COGS")
	approx(t, g.Stages[model.StageConsolidated][coord("Sales", model.OriginImport, "")], 1440, "Global consolidated Sales")
	if g.Input == nil {
		t.Error("Global unit Input is nil after reload")
	}

	// Workflow entry with full history.
	e := got.Workflow.Get(wfKey)
	if e.Status != workflow.StatusValidated {
		t.Errorf("workflow status = %q, want %q", e.Status, workflow.StatusValidated)
	}
	if len(e.History) != 2 {
		t.Fatalf("workflow history len = %d, want 2", len(e.History))
	}
	first := e.History[0]
	if first.Action != workflow.ActionImport || first.From != workflow.StatusNotStarted ||
		first.To != workflow.StatusImported || first.By != "kb" || !first.At.Equal(fixedAt) {
		t.Errorf("history[0] = %+v; want import NotStarted->Imported by kb at %v", first, fixedAt)
	}
	if !e.UpdatedAt.Equal(fixedAt.Add(time.Minute)) || e.UpdatedBy != "kb" {
		t.Errorf("entry updated = %v/%q; want %v/kb", e.UpdatedAt, e.UpdatedBy, fixedAt.Add(time.Minute))
	}

	// Whole-structure deep equality.
	if !reflect.DeepEqual(got.Meta, orig.Meta) {
		t.Error("Meta not deep-equal after round trip")
	}
	if !reflect.DeepEqual(got.Cells.Units, orig.Cells.Units) {
		t.Error("Cells.Units not deep-equal after round trip")
	}
	if !reflect.DeepEqual(got.Profiles, orig.Profiles) {
		t.Error("Profiles not deep-equal after round trip")
	}
	if !reflect.DeepEqual(got.Workflow.Entries, orig.Workflow.Entries) {
		t.Error("Workflow.Entries not deep-equal after round trip")
	}

	// Re-saving the loaded state must produce a byte-identical snapshot
	// (json.Marshal sorts map keys, so snapshots are deterministic).
	path2 := filepath.Join(dir, "resave.json")
	if err := Save(path2, got); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	raw2, err := os.ReadFile(path2)
	if err != nil {
		t.Fatalf("read re-saved snapshot: %v", err)
	}
	if !bytes.Equal(raw, raw2) {
		t.Error("re-saved snapshot differs from original snapshot bytes")
	}
}

func TestLoadMissingFile(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "no-such-dir", "onetrickle.json"))
	if err != nil {
		t.Fatalf("Load missing file: %v, want nil error", err)
	}
	if s == nil {
		t.Fatal("Load missing file returned nil state")
	}
	if s.Meta == nil || s.Cells == nil || s.Profiles == nil || s.Workflow == nil {
		t.Fatalf("fresh state has nil fields: %+v", s)
	}
	if !s.Meta.Dim(model.DimOrigin).Has(model.OriginImport) || !s.Meta.Dim(model.DimOrigin).Has(model.OriginElim) {
		t.Error("fresh state Origin dim missing fixed members")
	}
	if !s.Meta.Dim(model.DimFlow).Has(model.NoneMember) || !s.Meta.Dim(model.DimUD4).Has(model.NoneMember) {
		t.Error("fresh state Flow/UD dims missing None root")
	}
	if n := len(s.Cells.Units); n != 0 {
		t.Errorf("fresh state has %d units, want 0", n)
	}
	if len(s.Profiles) != 0 || s.Workflow.Entries == nil || len(s.Workflow.Entries) != 0 {
		t.Errorf("fresh state not empty: %d profiles, workflow entries %v", len(s.Profiles), s.Workflow.Entries)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		errPart string // substring the error must contain
	}{
		{"corrupted json", `{this is not json`, "parse snapshot"},
		{"truncated json", `{"version":1,"metadata":`, "parse snapshot"},
		{"json array", `[1,2,3]`, "parse snapshot"},
		{"non-numeric version", `{"version":"one"}`, "parse snapshot"},
		{"future version", `{"version":2}`, "unsupported snapshot version 2"},
		{"missing version", `{"metadata":null}`, "unsupported snapshot version 0"},
		{"json null", `null`, "unsupported snapshot version 0"},
		{"bad unit key", `{"version":1,"units":{"bad":{}}}`, "parse snapshot"},
		{"null unit", `{"version":1,"units":{"Golf|US|Actual|2025M1":null}}`, "parse snapshot"},
		{"bad cell coord", `{"version":1,"units":{"Golf|US|Actual|2025M1":{"input":{"short|key":1}}}}`, "parse snapshot"},
		{"bad workflow key", `{"version":1,"workflow":{"bad":{"status":"Imported"}}}`, "parse snapshot"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "onetrickle.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			s, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%s) succeeded, want error", tc.name)
			}
			if s != nil {
				t.Errorf("Load(%s) returned non-nil state with error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("error %q does not contain %q", err, tc.errPart)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the path %q", err, path)
			}
		})
	}
}

func TestLoadUnreadablePath(t *testing.T) {
	// A directory is neither missing nor readable as a file: must error.
	dir := t.TempDir()
	if _, err := Load(dir); err == nil {
		t.Error("Load(directory) succeeded, want error")
	}
}

// checkInitialized asserts every sub-structure a loaded state exposes is
// non-nil and usable (the server mutates these without nil checks).
func checkInitialized(t *testing.T, s *AppState) {
	t.Helper()
	if s.Meta == nil || s.Cells == nil || s.Profiles == nil || s.Workflow == nil {
		t.Fatalf("state has nil top-level fields: %+v", s)
	}
	if s.Meta.Cubes == nil || s.Meta.Dims == nil || s.Meta.Rates == nil {
		t.Fatalf("metadata has nil maps: %+v", s.Meta)
	}
	for name, c := range s.Meta.Cubes {
		if c == nil {
			t.Errorf("nil cube %q survived load", name)
		}
	}
	for _, dt := range model.AllDims {
		d := s.Meta.Dim(dt)
		if d == nil {
			t.Fatalf("dimension %s is nil after load", dt)
		}
		if d.Type != dt {
			t.Errorf("dimension %s has Type %q", dt, d.Type)
		}
		if d.Members == nil {
			t.Fatalf("dimension %s has nil Members", dt)
		}
		for name, m := range d.Members {
			if m == nil {
				t.Errorf("nil member %q survived in %s", name, dt)
			}
		}
	}
	if s.Cells.Units == nil {
		t.Fatal("Cells.Units is nil after load")
	}
	for k, u := range s.Cells.Units {
		if u == nil {
			t.Fatalf("nil unit %q survived load", k.Key())
		}
		if u.Input == nil {
			t.Errorf("unit %q has nil Input", k.Key())
		}
	}
	for name, p := range s.Profiles {
		if p == nil {
			t.Errorf("nil profile %q survived load", name)
		}
	}
	if s.Workflow.Entries == nil {
		t.Fatal("Workflow.Entries is nil after load")
	}

	// Mutation smoke tests: none of these may panic.
	if err := s.Meta.Entity().AddMember(&model.Member{Name: "SmokeEntity"}); err != nil {
		t.Errorf("AddMember on loaded state: %v", err)
	}
	if err := s.Meta.Rates.Set("S", "2025M1", "EUR", model.RateAverage, 1.5); err != nil {
		t.Errorf("Rates.Set on loaded state: %v", err)
	}
	u := s.Cells.Ensure(cube.UnitKey{Cube: "C", Entity: "E", Scenario: "S", Time: "2025M1"})
	u.Input[coord("A", model.OriginForms, "")] = 1
	s.Profiles["smoke"] = &stage.Profile{Name: "smoke"}
	k := workflow.Key{Cube: "C", Entity: "E", Scenario: "S", Time: "2025M1"}
	if _, err := s.Workflow.Apply(k, workflow.ActionImport, "t", fixedAt); err != nil {
		t.Errorf("Workflow.Apply on loaded state: %v", err)
	}
	if probs := s.Meta.Validate(); probs == nil {
		_ = probs // Validate must simply not panic; problems are acceptable.
	}
}

func TestLoadNilFieldResilience(t *testing.T) {
	tests := []struct {
		name string
		data string
	}{
		{"only version", `{"version":1}`},
		{"explicit nulls", `{"version":1,"metadata":null,"units":null,"profiles":null,"workflow":null}`},
		{"missing profiles", `{"version":1,"metadata":{"cubes":{},"dims":{},"rates":{}},"units":{},"workflow":{}}`},
		{"nil metadata maps", `{"version":1,"metadata":{"cubes":null,"dims":null,"rates":null}}`},
		{"nil dim and members", `{"version":1,"metadata":{"cubes":{"X":null},"dims":{"Entity":null,"Account":{"type":"Account","members":null,"roots":null},"Bogus":null},"rates":{}}}`},
		{"null members in dim", `{"version":1,"metadata":{"dims":{"Entity":{"type":"Entity","members":{"Ghost":null}}}}}`},
		{"null profile entry", `{"version":1,"profiles":{"dead":null}}`},
		{"unit without input", `{"version":1,"units":{"Golf|US|Actual|2025M1":{"stages":{"Consolidated":{"Sales|None|Calc|None|None|None|None|None":5}}}}}`},
		{"empty workflow object", `{"version":1,"workflow":{}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "onetrickle.json")
			if err := os.WriteFile(path, []byte(tc.data), 0o644); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			s, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			checkInitialized(t, s)
		})
	}
}

func TestLoadUnitWithoutInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onetrickle.json")
	data := `{"version":1,"units":{"Golf|US|Actual|2025M1":{"stages":{"Consolidated":{"Sales|None|Calc|None|None|None|None|None":5}}}}}`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	u := s.Cells.Unit(cube.UnitKey{Cube: "Golf", Entity: "US", Scenario: "Actual", Time: "2025M1"})
	if u == nil {
		t.Fatal("unit missing after load")
	}
	if u.Input == nil {
		t.Error("unit Input is nil after load")
	}
	c := cube.CellCoord{Account: "Sales", Origin: model.OriginCalc}.Normalize()
	approx(t, u.Stages[model.StageConsolidated][c], 5, "consolidated stage cell")
}

func TestSaveAtomicAndOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onetrickle.json")

	if err := Save(path, populatedState(t)); err != nil {
		t.Fatalf("first Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file exists after first Save (err=%v)", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat snapshot: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("snapshot is not a regular file: %v", info.Mode())
	}

	// Overwriting with a fresh state must fully replace the snapshot.
	if err := Save(path, NewAppState()); err != nil {
		t.Fatalf("overwrite Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file exists after overwrite Save (err=%v)", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load after overwrite: %v", err)
	}
	if len(s.Cells.Units) != 0 || len(s.Profiles) != 0 || len(s.Workflow.Entries) != 0 {
		t.Errorf("overwritten snapshot still has data: %d units, %d profiles, %d workflow entries",
			len(s.Cells.Units), len(s.Profiles), len(s.Workflow.Entries))
	}
}

func TestSaveNilState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "onetrickle.json")
	if err := Save(path, nil); err == nil {
		t.Error("Save(nil) succeeded, want error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Save(nil) created a file (err=%v)", err)
	}
}

func TestSaveTolerantOfNilFields(t *testing.T) {
	// A partially constructed state must still save, and load back usable.
	path := filepath.Join(t.TempDir(), "onetrickle.json")
	if err := Save(path, &AppState{}); err != nil {
		t.Fatalf("Save of zero AppState: %v", err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	checkInitialized(t, s)
}

func TestNewAppState(t *testing.T) {
	s := NewAppState()
	if s.Meta == nil || s.Cells == nil || s.Profiles == nil || s.Workflow == nil {
		t.Fatalf("NewAppState has nil fields: %+v", s)
	}
	if s.Cells.Units == nil || s.Workflow.Entries == nil {
		t.Error("NewAppState inner maps are nil")
	}
	if probs := s.Meta.Validate(); len(probs) != 0 {
		t.Errorf("fresh metadata invalid: %v", probs)
	}
}
