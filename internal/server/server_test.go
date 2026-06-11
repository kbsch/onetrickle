package server

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"onetrickle/internal/consol"
	"onetrickle/internal/cube"
	"onetrickle/internal/seed"
	"onetrickle/internal/store"
	"onetrickle/internal/workflow"
)

const eps = 1e-9

func approx(a, b float64) bool { return math.Abs(a-b) < eps }

// newTestServer builds a server over a freshly seeded in-memory state with a
// temp-dir snapshot path. It returns the handler, the state and the path.
func newTestServer(t *testing.T) (http.Handler, *store.AppState, string) {
	t.Helper()
	meta, cells, profiles, err := seed.Build()
	if err != nil {
		t.Fatalf("seed.Build: %v", err)
	}
	st := &store.AppState{
		Meta:     meta,
		Cells:    cells,
		Profiles: profiles,
		Workflow: workflow.NewRegistry(),
	}
	path := filepath.Join(t.TempDir(), "onetrickle.json")
	return New(st, path).Handler(), st, path
}

// doJSON performs a request with an optional JSON body.
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBody[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, want, rec.Body.String())
	}
}

// querySingle runs a one-cell query for (account, pov) and returns the value.
func querySingle(t *testing.T, h http.Handler, pov cube.POV, account string) float64 {
	t.Helper()
	req := cube.QueryRequest{
		Cube: pov.Cube,
		POV:  pov,
		Rows: []cube.AxisSpec{{Dim: "Account", Member: account}},
		Cols: []cube.AxisSpec{{Dim: "Time", Member: pov.Time}},
	}
	rec := doJSON(t, h, http.MethodPost, "/api/query", req)
	wantStatus(t, rec, http.StatusOK)
	res := decodeBody[cube.QueryResult](t, rec)
	if len(res.Cells) != 1 || len(res.Cells[0]) != 1 {
		t.Fatalf("query %s: unexpected grid shape %+v", account, res)
	}
	return res.Cells[0][0]
}

func TestHealth(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/health", nil)
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[map[string]bool](t, rec); !got["ok"] {
		t.Fatalf("health = %v, want ok:true", got)
	}
}

func TestMetaShape(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/meta", nil)
	wantStatus(t, rec, http.StatusOK)
	got := decodeBody[struct {
		Cubes      []string `json:"cubes"`
		Scenarios  []string `json:"scenarios"`
		Years      []int    `json:"years"`
		Currencies []string `json:"currencies"`
	}](t, rec)
	if len(got.Cubes) != 1 || got.Cubes[0] != "GolfTrickle" {
		t.Errorf("cubes = %v, want [GolfTrickle]", got.Cubes)
	}
	if want := []string{"Actual", "Budget"}; len(got.Scenarios) != 2 || got.Scenarios[0] != want[0] || got.Scenarios[1] != want[1] {
		t.Errorf("scenarios = %v, want %v", got.Scenarios, want)
	}
	if len(got.Years) != 3 || got.Years[0] != 2024 || got.Years[2] != 2026 {
		t.Errorf("years = %v, want [2024 2025 2026]", got.Years)
	}
	if want := []string{"CAD", "EUR", "USD"}; len(got.Currencies) != 3 || got.Currencies[0] != want[0] || got.Currencies[2] != want[2] {
		t.Errorf("currencies = %v, want %v", got.Currencies, want)
	}
}

func TestDimMembersTree(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/dims/Entity/members", nil)
	wantStatus(t, rec, http.StatusOK)
	members := decodeBody[[]memberDTO](t, rec)
	if len(members) != 7 {
		t.Fatalf("entity members = %d, want 7", len(members))
	}
	if members[0].Name != "GolfTrickle Inc" || members[0].Depth != 0 || members[0].IsLeaf {
		t.Errorf("first member = %+v, want root GolfTrickle Inc", members[0])
	}
	var france *memberDTO
	for i := range members {
		if members[i].Name == "France" {
			france = &members[i]
		}
	}
	if france == nil {
		t.Fatal("France not found in entity tree")
	}
	if !approx(france.OwnershipPct, 80) || france.Parent != "Europe" || france.Depth != 2 || !france.IsLeaf || france.Currency != "EUR" {
		t.Errorf("France = %+v, want ownershipPct 80, parent Europe, depth 2, leaf, EUR", *france)
	}

	t.Run("errors", func(t *testing.T) {
		cases := []struct {
			method, path string
			body         any
			want         int
		}{
			{http.MethodGet, "/api/dims/Bogus/members", nil, http.StatusNotFound},
			{http.MethodPost, "/api/dims/Time/members", memberCreate{Name: "2099"}, http.StatusForbidden},
			{http.MethodPost, "/api/dims/Origin/members", memberCreate{Name: "X"}, http.StatusForbidden},
			{http.MethodDelete, "/api/dims/Origin/members/Import", nil, http.StatusForbidden},
			{http.MethodGet, "/api/nope", nil, http.StatusNotFound},
		}
		for _, c := range cases {
			rec := doJSON(t, h, c.method, c.path, c.body)
			if rec.Code != c.want {
				t.Errorf("%s %s: status = %d, want %d (body %s)", c.method, c.path, rec.Code, c.want, rec.Body.String())
			}
			if e := decodeBody[map[string]string](t, rec); e["error"] == "" {
				t.Errorf("%s %s: missing JSON error body: %s", c.method, c.path, rec.Body.String())
			}
		}
	})
}

func TestDimMemberCRUD(t *testing.T) {
	h, st, _ := newTestServer(t)

	rec := doJSON(t, h, http.MethodPost, "/api/dims/Entity/members",
		memberCreate{Name: "Spain", Parent: "Europe", Currency: "EUR"})
	wantStatus(t, rec, http.StatusOK)
	created := decodeBody[memberDTO](t, rec)
	if created.Depth != 2 || !approx(created.OwnershipPct, 100) || !approx(created.Weight, 1) {
		t.Errorf("created = %+v, want depth 2, ownership 100, weight 1", created)
	}

	// Duplicate add fails.
	rec = doJSON(t, h, http.MethodPost, "/api/dims/Entity/members", memberCreate{Name: "Spain", Parent: "Europe"})
	wantStatus(t, rec, http.StatusBadRequest)

	// Partial update: only ownershipPct changes.
	pct := 75.0
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Entity/members/Spain", memberUpdate{OwnershipPct: &pct})
	wantStatus(t, rec, http.StatusOK)
	updated := decodeBody[memberDTO](t, rec)
	if !approx(updated.OwnershipPct, 75) || updated.Currency != "EUR" {
		t.Errorf("updated = %+v, want ownership 75 and currency EUR kept", updated)
	}

	// Move via parent field.
	parent := "North America"
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Entity/members/Spain", memberUpdate{Parent: &parent})
	wantStatus(t, rec, http.StatusOK)
	if got := st.Meta.Entity().Get("Spain").Parent; got != "North America" {
		t.Errorf("Spain parent = %q, want North America", got)
	}

	// Delete leaf, then verify it is gone.
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/Entity/members/Spain", nil)
	wantStatus(t, rec, http.StatusOK)
	if st.Meta.Entity().Has("Spain") {
		t.Error("Spain still exists after delete")
	}

	// Delete with children requires recursive=1.
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/Entity/members/Europe", nil)
	wantStatus(t, rec, http.StatusBadRequest)
	// Recursive delete is refused while descendants still hold stored data.
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/Entity/members/Europe?recursive=1", nil)
	wantStatus(t, rec, http.StatusConflict)
	if !st.Meta.Entity().Has("Germany") {
		t.Error("Germany vanished after a refused recursive delete of Europe")
	}

	// Account validation: bad type, bad formula.
	rec = doJSON(t, h, http.MethodPost, "/api/dims/Account/members", memberCreate{Name: "X1", AccountType: "Bogus"})
	wantStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodPost, "/api/dims/Account/members", memberCreate{Name: "X1", Formula: "A#Sales +"})
	wantStatus(t, rec, http.StatusBadRequest)

	// Update of a missing member is a 404.
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Entity/members/Nowhere", memberUpdate{})
	wantStatus(t, rec, http.StatusNotFound)
}

func TestQueryConsolidated(t *testing.T) {
	h, st, _ := newTestServer(t)
	if _, err := consol.Process(st.Meta, st.Cells, "GolfTrickle", "Actual", "2025M1"); err != nil {
		t.Fatalf("consol.Process: %v", err)
	}
	pov := cube.POV{
		Cube: "GolfTrickle", Entity: "GolfTrickle Inc", Scenario: "Actual",
		Time: "2025M1", Stage: "Consolidated",
	}
	got := querySingle(t, h, pov, "Sales")
	if got <= 0 {
		t.Fatalf("Global consolidated Sales = %v, want > 0", got)
	}
	// US 1200 (incl IC 200) + Canada 600*0.74 + Germany 850*1.09 +
	// France 500*1.09*0.8 - IC elimination 200 = 2806.5.
	if want := 2806.5; !approx(got, want) {
		t.Errorf("Global consolidated Sales = %v, want %v", got, want)
	}
	// Dynamic calc resolves through the wired DynEval.
	gp := querySingle(t, h, pov, "GrossProfit")
	sales := querySingle(t, h, pov, "Sales")
	margin := querySingle(t, h, pov, "GPMargin")
	if sales == 0 || !approx(margin, gp/sales*100) {
		t.Errorf("GPMargin = %v, want %v (gp %v / sales %v * 100)", margin, gp/sales*100, gp, sales)
	}
}

func TestDataCellsWriteRead(t *testing.T) {
	h, _, path := newTestServer(t)
	unit := cube.UnitKey{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M8"}
	coord := cube.CellCoord{Account: "Sales", Origin: "Forms"}

	rec := doJSON(t, h, http.MethodPost, "/api/data/cells",
		[]cube.CellWrite{{Unit: unit, Coord: coord, Value: 123.45}})
	wantStatus(t, rec, http.StatusOK)

	pov := cube.POV{Cube: unit.Cube, Entity: unit.Entity, Scenario: unit.Scenario, Time: unit.Time, Stage: "Local"}
	if got := querySingle(t, h, pov, "Sales"); !approx(got, 123.45) {
		t.Errorf("read back = %v, want 123.45", got)
	}

	// The mutation was persisted before replying.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("snapshot not saved: %v", err)
	}
	loaded, err := store.Load(path)
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	u := loaded.Cells.Unit(unit)
	if u == nil || !approx(u.Input[coord.Normalize()], 123.45) {
		t.Errorf("persisted cell missing; unit=%+v", u)
	}

	// Zero deletes.
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells",
		[]cube.CellWrite{{Unit: unit, Coord: coord, Value: 0}})
	wantStatus(t, rec, http.StatusOK)
	if got := querySingle(t, h, pov, "Sales"); got != 0 {
		t.Errorf("after delete = %v, want 0", got)
	}

	t.Run("validation", func(t *testing.T) {
		bad := []struct {
			name string
			wr   cube.CellWrite
		}{
			{"unknown cube", cube.CellWrite{Unit: cube.UnitKey{Cube: "Nope", Entity: "US Operations", Scenario: "Actual", Time: "2025M8"}, Coord: coord, Value: 1}},
			{"non-leaf entity", cube.CellWrite{Unit: cube.UnitKey{Cube: "GolfTrickle", Entity: "North America", Scenario: "Actual", Time: "2025M8"}, Coord: coord, Value: 1}},
			{"unknown account", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "Nope", Origin: "Forms"}, Value: 1}},
			{"non-leaf account", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "OpEx", Origin: "Forms"}, Value: 1}},
			{"dynamic-calc account", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "GPMargin", Origin: "Forms"}, Value: 1}},
			{"stored-calc account", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "GrossProfit", Origin: "Forms"}, Value: 1}},
			{"engine origin", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "Sales", Origin: "Calc"}, Value: 1}},
			{"empty origin", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "Sales"}, Value: 1}},
			{"bad IC", cube.CellWrite{Unit: unit, Coord: cube.CellCoord{Account: "Sales", Origin: "Forms", IC: "Mars"}, Value: 1}},
			{"quarter time", cube.CellWrite{Unit: cube.UnitKey{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025Q1"}, Coord: coord, Value: 1}},
		}
		for _, c := range bad {
			rec := doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{c.wr})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400 (body %s)", c.name, rec.Code, rec.Body.String())
			}
		}
	})

	// All-or-nothing: a batch with one bad write applies nothing.
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{
		{Unit: unit, Coord: coord, Value: 55},
		{Unit: unit, Coord: cube.CellCoord{Account: "Nope", Origin: "Forms"}, Value: 1},
	})
	wantStatus(t, rec, http.StatusBadRequest)
	if got := querySingle(t, h, pov, "Sales"); got != 0 {
		t.Errorf("partial batch applied: Sales = %v, want 0", got)
	}
}

func TestWorkflowSequenceAndCertifiedWrite(t *testing.T) {
	h, _, _ := newTestServer(t)
	act := func(entity, action string) *httptest.ResponseRecorder {
		return doJSON(t, h, http.MethodPost, "/api/workflow/action", workflowActionReq{
			Cube: "GolfTrickle", Entity: entity, Scenario: "Actual", Time: "2025M1", Action: action,
		})
	}

	// Invalid: certify out of order.
	rec := act("Canada", "certify")
	wantStatus(t, rec, http.StatusConflict)

	// Valid pipeline.
	for _, a := range []string{"import", "validate", "process", "certify"} {
		rec := act("Canada", a)
		wantStatus(t, rec, http.StatusOK)
		got := decodeBody[struct {
			Entry  workflow.Entry `json:"entry"`
			Issues []string       `json:"issues"`
		}](t, rec)
		if got.Entry.Key.Entity != "Canada" {
			t.Errorf("action %s: entry key = %+v", a, got.Entry.Key)
		}
		if a == "process" && got.Issues == nil {
			t.Errorf("process: issues is null, want []")
		}
	}

	// Certified unit rejects data writes.
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{{
		Unit:  cube.UnitKey{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025M1"},
		Coord: cube.CellCoord{Account: "Sales", Origin: "Forms"},
		Value: 9,
	}})
	wantStatus(t, rec, http.StatusConflict)

	// Reopen unlocks it again.
	rec = act("Canada", "reopen")
	wantStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{{
		Unit:  cube.UnitKey{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025M1"},
		Coord: cube.CellCoord{Account: "Sales", Origin: "Forms"},
		Value: 9,
	}})
	wantStatus(t, rec, http.StatusOK)

	// Bad refs are 400s.
	for _, body := range []workflowActionReq{
		{Cube: "Nope", Entity: "Canada", Scenario: "Actual", Time: "2025M1", Action: "import"},
		{Cube: "GolfTrickle", Entity: "Mars", Scenario: "Actual", Time: "2025M1", Action: "import"},
		{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Nope", Time: "2025M1", Action: "import"},
		{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025Q1", Action: "import"},
		{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025M1", Action: ""},
	} {
		rec := doJSON(t, h, http.MethodPost, "/api/workflow/action", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %+v: status = %d, want 400", body, rec.Code)
		}
	}
}

func TestWorkflowList(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/workflow?cube=GolfTrickle&scenario=Actual&time=2025M1", nil)
	wantStatus(t, rec, http.StatusOK)
	entries := decodeBody[[]workflowEntryDTO](t, rec)
	if len(entries) != 7 {
		t.Fatalf("entries = %d, want 7 (all entities, pre-order)", len(entries))
	}
	if entries[0].Entity != "GolfTrickle Inc" || entries[0].IsLeaf {
		t.Errorf("first entry = %+v, want non-leaf GolfTrickle Inc", entries[0])
	}
	byName := map[string]workflowEntryDTO{}
	for _, e := range entries {
		byName[e.Entity] = e
		if e.Status != workflow.StatusNotStarted {
			t.Errorf("%s status = %s, want NotStarted", e.Entity, e.Status)
		}
		if e.Key.Cube != "GolfTrickle" || e.Key.Scenario != "Actual" || e.Key.Time != "2025M1" {
			t.Errorf("%s key = %+v", e.Entity, e.Key)
		}
	}
	if !byName["US Operations"].IsLeaf || byName["Europe"].IsLeaf {
		t.Errorf("leaf flags wrong: US Operations=%v Europe=%v", byName["US Operations"].IsLeaf, byName["Europe"].IsLeaf)
	}

	// After an action the status shows up.
	doJSON(t, h, http.MethodPost, "/api/workflow/action", workflowActionReq{
		Cube: "GolfTrickle", Entity: "Germany", Scenario: "Actual", Time: "2025M1", Action: "import",
	})
	rec = doJSON(t, h, http.MethodGet, "/api/workflow?cube=GolfTrickle&scenario=Actual&time=2025M1", nil)
	entries = decodeBody[[]workflowEntryDTO](t, rec)
	for _, e := range entries {
		if e.Entity == "Germany" && e.Status != workflow.StatusImported {
			t.Errorf("Germany status = %s, want Imported", e.Status)
		}
	}

	// Missing params.
	rec = doJSON(t, h, http.MethodGet, "/api/workflow?cube=GolfTrickle", nil)
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestFormulas(t *testing.T) {
	h, st, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/formulas?cube=GolfTrickle", nil)
	wantStatus(t, rec, http.StatusOK)
	list := decodeBody[[]formulaDTO](t, rec)
	byAcct := map[string]formulaDTO{}
	for _, f := range list {
		byAcct[f.Account] = f
	}
	if len(list) != 2 {
		t.Fatalf("formulas = %+v, want GPMargin and GrossProfit", list)
	}
	if f := byAcct["GrossProfit"]; f.Dynamic || f.Formula != "A#Sales - A#COGS" {
		t.Errorf("GrossProfit = %+v", f)
	}
	if f := byAcct["GPMargin"]; !f.Dynamic {
		t.Errorf("GPMargin = %+v, want dynamic", f)
	}

	// Bad formula is a 400 and changes nothing.
	rec = doJSON(t, h, http.MethodPut, "/api/formulas/GPMargin",
		map[string]any{"formula": "A#Sales + ", "dynamic": true})
	wantStatus(t, rec, http.StatusBadRequest)
	if got := st.Meta.Account().Get("GPMargin").Formula; !strings.Contains(got, "GrossProfit") {
		t.Errorf("GPMargin formula overwritten by failed PUT: %q", got)
	}

	// Unknown account is a 404.
	rec = doJSON(t, h, http.MethodPut, "/api/formulas/Nope", map[string]any{"formula": "1+1", "dynamic": false})
	wantStatus(t, rec, http.StatusNotFound)

	// Good formula saves.
	rec = doJSON(t, h, http.MethodPut, "/api/formulas/GPMargin",
		map[string]any{"formula": "A#Sales * 2", "dynamic": true})
	wantStatus(t, rec, http.StatusOK)
	m := st.Meta.Account().Get("GPMargin")
	if m.Formula != "A#Sales * 2" || !m.DynamicCalc {
		t.Errorf("GPMargin after PUT = %+v", m)
	}

	// Empty formula clears formula and dynamic flag.
	rec = doJSON(t, h, http.MethodPut, "/api/formulas/GPMargin", map[string]any{"formula": "", "dynamic": true})
	wantStatus(t, rec, http.StatusOK)
	if m.Formula != "" || m.DynamicCalc {
		t.Errorf("GPMargin after clear = %+v", m)
	}
}

func TestRates(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/rates?scenario=Actual&time=2025M1", nil)
	wantStatus(t, rec, http.StatusOK)
	rates := decodeBody[[]rateDTO](t, rec)
	if len(rates) != 4 {
		t.Fatalf("rates = %d, want 4 (CAD/EUR x Average/Closing)", len(rates))
	}
	found := false
	for _, r := range rates {
		if r.Currency == "EUR" && r.Type == "Average" {
			found = true
			if !approx(r.Value, 1.09) {
				t.Errorf("EUR Average = %v, want 1.09", r.Value)
			}
		}
	}
	if !found {
		t.Error("EUR Average rate missing")
	}

	for _, c := range []struct {
		name string
		url  string
		body any
		want int
	}{
		{"non-positive value", "/api/rates?scenario=Actual&time=2025M1", []rateDTO{{Currency: "GBP", Type: "Average", Value: 0}}, http.StatusBadRequest},
		{"bad type", "/api/rates?scenario=Actual&time=2025M1", []rateDTO{{Currency: "GBP", Type: "Spot", Value: 1}}, http.StatusBadRequest},
		{"unknown scenario", "/api/rates?scenario=Nope&time=2025M1", []rateDTO{{Currency: "GBP", Type: "Average", Value: 1}}, http.StatusBadRequest},
		{"non-month time", "/api/rates?scenario=Actual&time=2025", []rateDTO{{Currency: "GBP", Type: "Average", Value: 1}}, http.StatusBadRequest},
	} {
		rec := doJSON(t, h, http.MethodPut, c.url, c.body)
		if rec.Code != c.want {
			t.Errorf("%s: status = %d, want %d (body %s)", c.name, rec.Code, c.want, rec.Body.String())
		}
	}

	rec = doJSON(t, h, http.MethodPut, "/api/rates?scenario=Actual&time=2025M1",
		[]rateDTO{{Currency: "GBP", Type: "Average", Value: 1.27}})
	wantStatus(t, rec, http.StatusOK)
	rates = decodeBody[[]rateDTO](t, rec)
	if len(rates) != 5 {
		t.Fatalf("rates after PUT = %d, want 5", len(rates))
	}
}

// multipartBody builds an /api/import request body with profile + file fields.
func multipartBody(t *testing.T, profile string, file []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("profile", profile); err != nil {
		t.Fatalf("write profile field: %v", err)
	}
	fw, err := mw.CreateFormFile("file", "sample.csv")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := fw.Write(file); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return &buf, mw.FormDataContentType()
}

func doMultipart(t *testing.T, h http.Handler, path, profile string, file []byte) *httptest.ResponseRecorder {
	t.Helper()
	body, ctype := multipartBody(t, profile, file)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", ctype)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestImportPreviewAndCommit(t *testing.T) {
	h, _, _ := newTestServer(t)

	rec := doMultipart(t, h, "/api/import/preview", seed.ProfileName, seed.SampleCSV())
	wantStatus(t, rec, http.StatusOK)
	prev := decodeBody[struct {
		Rows      []map[string]any `json:"rows"`
		Issues    []string         `json:"issues"`
		TotalRows int              `json:"totalRows"`
		CleanRows int              `json:"cleanRows"`
	}](t, rec)
	if prev.TotalRows != 6 || prev.CleanRows != 6 || len(prev.Rows) != 6 || len(prev.Issues) != 0 {
		t.Fatalf("preview = total %d clean %d rows %d issues %v", prev.TotalRows, prev.CleanRows, len(prev.Rows), prev.Issues)
	}
	if acct := prev.Rows[0]["account"]; acct != "Sales" {
		t.Errorf("row 0 account = %v, want Sales (rule 4100 -> Sales)", acct)
	}

	// Unknown profile is a 404.
	rec = doMultipart(t, h, "/api/import/preview", "Nope", seed.SampleCSV())
	wantStatus(t, rec, http.StatusNotFound)

	rec = doMultipart(t, h, "/api/import/commit", seed.ProfileName, seed.SampleCSV())
	wantStatus(t, rec, http.StatusOK)
	com := decodeBody[struct {
		UnitsLoaded  int      `json:"unitsLoaded"`
		CellsWritten int      `json:"cellsWritten"`
		SkippedRows  int      `json:"skippedRows"`
		Issues       []string `json:"issues"`
	}](t, rec)
	if com.UnitsLoaded != 1 || com.CellsWritten != 2 || com.SkippedRows != 0 {
		t.Fatalf("commit = %+v, want 1 unit, 2 cells, 0 skipped", com)
	}

	// Loaded data is queryable (sums of mapped rows).
	pov := cube.POV{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual",
		Time: "2025M7", Stage: "Local", Origin: "Import"}
	if got := querySingle(t, h, pov, "Sales"); !approx(got, 3200) {
		t.Errorf("imported Sales = %v, want 3200", got)
	}
	if got := querySingle(t, h, pov, "COGS"); !approx(got, 1615) {
		t.Errorf("imported COGS = %v, want 1615", got)
	}

	// Workflow advanced to Imported for the loaded unit.
	rec = doJSON(t, h, http.MethodGet, "/api/workflow?cube=GolfTrickle&scenario=Actual&time=2025M7", nil)
	entries := decodeBody[[]workflowEntryDTO](t, rec)
	for _, e := range entries {
		if e.Entity == "US Operations" && e.Status != workflow.StatusImported {
			t.Errorf("US Operations 2025M7 status = %s, want Imported", e.Status)
		}
	}

	// Replace semantics: re-committing does not double the values.
	rec = doMultipart(t, h, "/api/import/commit", seed.ProfileName, seed.SampleCSV())
	wantStatus(t, rec, http.StatusOK)
	if got := querySingle(t, h, pov, "Sales"); !approx(got, 3200) {
		t.Errorf("after re-commit Sales = %v, want 3200 (replace, not add)", got)
	}

	// A certified target unit refuses the commit.
	for _, a := range []string{"validate", "process", "certify"} {
		rec := doJSON(t, h, http.MethodPost, "/api/workflow/action", workflowActionReq{
			Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M7", Action: a,
		})
		wantStatus(t, rec, http.StatusOK)
	}
	rec = doMultipart(t, h, "/api/import/commit", seed.ProfileName, seed.SampleCSV())
	wantStatus(t, rec, http.StatusConflict)
}

func TestExportCSV(t *testing.T) {
	h, st, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/export?cube=GolfTrickle&scenario=Actual&time=2025M1&stage=Local", nil)
	wantStatus(t, rec, http.StatusOK)
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want text/csv", ct)
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "onetrickle-export.csv") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if lines[0] != "entity,account,flow,origin,ic,ud1,ud2,ud3,ud4,value" {
		t.Fatalf("header = %q", lines[0])
	}
	// 4 entities x 8 accounts + 2 IC cells = 34 data lines.
	if len(lines)-1 != 34 {
		t.Errorf("data lines = %d, want 34", len(lines)-1)
	}
	wantLine := "US Operations,Sales,None,Import,Germany,None,None,None,None,200"
	found := false
	for _, l := range lines[1:] {
		if l == wantLine {
			found = true
		}
	}
	if !found {
		t.Errorf("export missing line %q in:\n%s", wantLine, rec.Body.String())
	}

	// Consolidated stage export after Process is non-empty.
	if _, err := consol.Process(st.Meta, st.Cells, "GolfTrickle", "Actual", "2025M1"); err != nil {
		t.Fatalf("consol.Process: %v", err)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/export?cube=GolfTrickle&scenario=Actual&time=2025M1", nil)
	wantStatus(t, rec, http.StatusOK)
	if n := strings.Count(rec.Body.String(), "\n"); n < 10 {
		t.Errorf("consolidated export has %d lines, want > 10", n)
	}

	// Errors.
	rec = doJSON(t, h, http.MethodGet, "/api/export?cube=GolfTrickle&scenario=Actual&time=2025M1&stage=Nope", nil)
	wantStatus(t, rec, http.StatusBadRequest)
	rec = doJSON(t, h, http.MethodGet, "/api/export?cube=GolfTrickle", nil)
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestProfilesCRUD(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/profiles", nil)
	wantStatus(t, rec, http.StatusOK)
	list := decodeBody[[]map[string]any](t, rec)
	if len(list) != 1 || list[0]["name"] != seed.ProfileName {
		t.Fatalf("profiles = %+v, want [%s]", list, seed.ProfileName)
	}

	// POST duplicate name conflicts.
	rec = doJSON(t, h, http.MethodPost, "/api/profiles", map[string]any{"name": seed.ProfileName})
	wantStatus(t, rec, http.StatusConflict)
	// POST without a name is a 400.
	rec = doJSON(t, h, http.MethodPost, "/api/profiles", map[string]any{"cube": "GolfTrickle"})
	wantStatus(t, rec, http.StatusBadRequest)

	// POST new, PUT update, DELETE.
	rec = doJSON(t, h, http.MethodPost, "/api/profiles", map[string]any{"name": "P2", "cube": "GolfTrickle"})
	wantStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodPut, "/api/profiles/P2", map[string]any{"name": "ignored", "cube": "GolfTrickle", "hasHeader": true})
	wantStatus(t, rec, http.StatusOK)
	if got := decodeBody[map[string]any](t, rec); got["name"] != "P2" || got["hasHeader"] != true {
		t.Errorf("PUT result = %+v, want path name P2 with hasHeader", got)
	}
	rec = doJSON(t, h, http.MethodDelete, "/api/profiles/P2", nil)
	wantStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodDelete, "/api/profiles/P2", nil)
	wantStatus(t, rec, http.StatusNotFound)
}

func TestProcessEndpoint(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodPost, "/api/process",
		map[string]string{"cube": "GolfTrickle", "scenario": "Actual", "time": "2025M2"})
	wantStatus(t, rec, http.StatusOK)
	res := decodeBody[consol.Result](t, rec)
	if res.UnitsProcessed <= 0 || res.CellsWritten <= 0 {
		t.Errorf("process result = %+v, want positive counts", res)
	}
	if res.Issues == nil {
		t.Error("issues is null, want []")
	}

	rec = doJSON(t, h, http.MethodPost, "/api/process",
		map[string]string{"cube": "Nope", "scenario": "Actual", "time": "2025M2"})
	wantStatus(t, rec, http.StatusBadRequest)
}

func TestStaticUI(t *testing.T) {
	h, _, _ := newTestServer(t)
	for _, c := range []struct {
		path, ctype, marker string
	}{
		{"/", "text/html", "<html"},
		{"/app.js", "text/javascript", ""},
		{"/style.css", "text/css", ""},
	} {
		rec := doJSON(t, h, http.MethodGet, c.path, nil)
		wantStatus(t, rec, http.StatusOK)
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, c.ctype) {
			t.Errorf("%s Content-Type = %q, want %s", c.path, ct, c.ctype)
		}
		if c.marker != "" && !strings.Contains(strings.ToLower(rec.Body.String()), c.marker) {
			t.Errorf("%s body missing %q", c.path, c.marker)
		}
	}
}
