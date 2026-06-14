package server

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/store"
	"onetrickle/internal/workflow"
)

// TestWorkflowRejectedActionHasNoSideEffects pins that an illegal workflow
// "process" action (409) does NOT run the consolidation: previously the
// full-cube Process executed before the transition check, leaving in-memory
// stages that diverged from the on-disk snapshot.
func TestWorkflowRejectedActionHasNoSideEffects(t *testing.T) {
	h, st, _ := newTestServer(t)
	pov := cube.POV{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual",
		Time: "2025M5", Stage: "Translated"}
	if got := querySingle(t, h, pov, "Sales"); got != 0 {
		t.Fatalf("pre-action Translated Sales = %v, want 0 (2025M5 never processed)", got)
	}

	rec := doJSON(t, h, http.MethodPost, "/api/workflow/action", workflowActionReq{
		Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M5", Action: "process",
	})
	wantStatus(t, rec, http.StatusConflict)

	if got := querySingle(t, h, pov, "Sales"); got != 0 {
		t.Errorf("post-409 Translated Sales = %v, want 0 (rejected action must not consolidate)", got)
	}
	u := st.Cells.Unit(cube.UnitKey{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M5"})
	if u != nil && len(u.Stages) != 0 {
		t.Errorf("unit gained stages from a rejected action: %v", u.Stages)
	}

	// Other actions are still rejected with 409 and the same message shape.
	rec = doJSON(t, h, http.MethodPost, "/api/workflow/action", workflowActionReq{
		Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M5", Action: "validate",
	})
	wantStatus(t, rec, http.StatusConflict)
	if e := decodeBody[map[string]string](t, rec); !strings.Contains(e["error"], "not allowed") {
		t.Errorf("error = %q, want transition error", e["error"])
	}
}

// TestImportNonFiniteAmounts pins that NaN/Inf CSV amounts never reach the
// store: a single bad row is flagged and skipped, and rows whose finite
// amounts SUM to Inf abort the commit — previously one such cell made every
// later snapshot save fail with 500 (silent data loss on restart).
func TestImportNonFiniteAmounts(t *testing.T) {
	h, _, _ := newTestServer(t)

	csv := "entity,account,time,amount\nUS Operations,4100,2025M7,Inf\nUS Operations,4100,2025M7,25\n"
	rec := doMultipart(t, h, "/api/import/preview", "GolfTrickle CSV", []byte(csv))
	wantStatus(t, rec, http.StatusOK)
	prev := decodeBody[struct {
		CleanRows int      `json:"cleanRows"`
		TotalRows int      `json:"totalRows"`
		Issues    []string `json:"issues"`
	}](t, rec)
	if prev.TotalRows != 2 || prev.CleanRows != 1 {
		t.Errorf("preview = %+v, want 2 total / 1 clean", prev)
	}

	rec = doMultipart(t, h, "/api/import/commit", "GolfTrickle CSV", []byte(csv))
	wantStatus(t, rec, http.StatusOK)
	com := decodeBody[struct {
		SkippedRows  int `json:"skippedRows"`
		CellsWritten int `json:"cellsWritten"`
	}](t, rec)
	if com.SkippedRows != 1 || com.CellsWritten != 1 {
		t.Errorf("commit = %+v, want 1 skipped / 1 written", com)
	}

	// Finite per-row amounts summing to +Inf abort the whole commit.
	overflow := "entity,account,time,amount\nUS Operations,4100,2025M8,1e308\nUS Operations,4100,2025M8,1e308\n"
	rec = doMultipart(t, h, "/api/import/commit", "GolfTrickle CSV", []byte(overflow))
	wantStatus(t, rec, http.StatusBadRequest)
	if e := decodeBody[map[string]string](t, rec); !strings.Contains(e["error"], "not finite") {
		t.Errorf("error = %q, want non-finite sum error", e["error"])
	}

	// Persistence is healthy: an unrelated valid write still saves (200).
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{{
		Unit:  cube.UnitKey{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Budget", Time: "2025M6"},
		Coord: cube.CellCoord{Account: "Rent", Origin: "Forms"},
		Value: 777,
	}})
	wantStatus(t, rec, http.StatusOK)
}

// TestValidateWriteNonFinite pins the defense-in-depth check on the cell
// write path (JSON itself cannot carry NaN/Inf, but internal callers can).
func TestValidateWriteNonFinite(t *testing.T) {
	_, st, _ := newTestServer(t)
	wr := cube.CellWrite{
		Unit:  cube.UnitKey{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025M8"},
		Coord: cube.CellCoord{Account: "Sales", Origin: "Forms"}.Normalize(),
		Value: math.NaN(),
	}
	if err := validateWrite(st.Meta, st.Workflow, wr); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Errorf("validateWrite(NaN) = %v, want finite-value error", err)
	}
	wr.Value = math.Inf(1)
	if err := validateWrite(st.Meta, st.Workflow, wr); err == nil || !strings.Contains(err.Error(), "finite") {
		t.Errorf("validateWrite(+Inf) = %v, want finite-value error", err)
	}
}

// TestImportUploadTooLarge pins the http.MaxBytesReader bound on import
// uploads: a body over the limit is rejected with 413 before parsing.
func TestImportUploadTooLarge(t *testing.T) {
	h, _, _ := newTestServer(t)
	big := bytes.Repeat([]byte("x"), maxImportUpload+1)
	rec := doMultipart(t, h, "/api/import/preview", "GolfTrickle CSV", big)
	wantStatus(t, rec, http.StatusRequestEntityTooLarge)
	if e := decodeBody[map[string]string](t, rec); !strings.Contains(e["error"], "limit") {
		t.Errorf("error = %q, want size-limit error", e["error"])
	}
}

// TestDimDeleteReferentialIntegrity pins that deleting a member still
// referenced by stored cells is refused (409), so no orphan cells survive in
// exports/queries or resurrect when the member is re-created.
func TestDimDeleteReferentialIntegrity(t *testing.T) {
	h, st, _ := newTestServer(t)

	// UD1 member with a stored cell.
	rec := doJSON(t, h, http.MethodPost, "/api/dims/UD1/members", memberCreate{Name: "RegionX"})
	wantStatus(t, rec, http.StatusOK)
	unit := cube.UnitKey{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M8"}
	coord := cube.CellCoord{Account: "Sales", Origin: "Forms", UD1: "RegionX"}
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{{Unit: unit, Coord: coord, Value: 999}})
	wantStatus(t, rec, http.StatusOK)

	rec = doJSON(t, h, http.MethodDelete, "/api/dims/UD1/members/RegionX", nil)
	wantStatus(t, rec, http.StatusConflict)
	if !st.Meta.Dim(model.DimUD1).Has("RegionX") {
		t.Fatal("RegionX vanished despite the 409")
	}

	// Clearing the cell unblocks the delete.
	rec = doJSON(t, h, http.MethodPost, "/api/data/cells", []cube.CellWrite{{Unit: unit, Coord: coord, Value: 0}})
	wantStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/UD1/members/RegionX", nil)
	wantStatus(t, rec, http.StatusOK)

	// An entity referenced as IC partner by other units' cells is protected
	// too (seed: US Operations Sales[IC=Germany]).
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/Entity/members/Germany", nil)
	wantStatus(t, rec, http.StatusConflict)
	if !st.Meta.Entity().Has("Germany") {
		t.Fatal("Germany vanished despite the 409")
	}

	// A data-free entity deletes fine, and its derived-only unit (stages
	// written by Process at the FCA) would be purged with it.
	rec = doJSON(t, h, http.MethodPost, "/api/dims/Entity/members",
		memberCreate{Name: "Shell", Parent: "Europe", Currency: "EUR"})
	wantStatus(t, rec, http.StatusOK)
	rec = doJSON(t, h, http.MethodDelete, "/api/dims/Entity/members/Shell", nil)
	wantStatus(t, rec, http.StatusOK)
}

// TestDimDeleteNoneProtected pins that the mandatory None roots of Flow and
// UD1..4 can be neither deleted nor re-parented: every stored cell normalizes
// empty coordinates to None, so removing it bricks queries, writes & imports.
func TestDimDeleteNoneProtected(t *testing.T) {
	h, _, _ := newTestServer(t)
	for _, dim := range []string{"Flow", "UD1", "UD2", "UD3", "UD4"} {
		rec := doJSON(t, h, http.MethodDelete, "/api/dims/"+dim+"/members/None", nil)
		wantStatus(t, rec, http.StatusBadRequest)
	}
	// Moving None under a parent is refused too.
	rec := doJSON(t, h, http.MethodPost, "/api/dims/Flow/members", memberCreate{Name: "Open"})
	wantStatus(t, rec, http.StatusOK)
	parent := "Open"
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Flow/members/None", memberUpdate{Parent: &parent})
	wantStatus(t, rec, http.StatusBadRequest)
}

// TestScenarioStaysFlat pins SPEC §2: the Scenario dimension is flat, so
// members cannot be created with (or moved under) a parent.
func TestScenarioStaysFlat(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodPost, "/api/dims/Scenario/members",
		memberCreate{Name: "ActualChild", Parent: "Actual"})
	wantStatus(t, rec, http.StatusBadRequest)

	rec = doJSON(t, h, http.MethodPost, "/api/dims/Scenario/members", memberCreate{Name: "Forecast"})
	wantStatus(t, rec, http.StatusOK)
	parent := "Actual"
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Scenario/members/Forecast", memberUpdate{Parent: &parent})
	wantStatus(t, rec, http.StatusBadRequest)
}

// TestStoredFormulaLeafOnly pins that stored-calc formulas may only target
// leaf accounts, on every mutation path: PUT /api/formulas, member update,
// and adding/moving children under a stored-calc account.
func TestStoredFormulaLeafOnly(t *testing.T) {
	h, st, _ := newTestServer(t)

	// Stored formula on a non-leaf account (OpEx has children) is refused...
	rec := doJSON(t, h, http.MethodPut, "/api/formulas/OpEx",
		map[string]any{"formula": "A#Sales - A#COGS", "dynamic": false})
	wantStatus(t, rec, http.StatusBadRequest)
	if got := st.Meta.Account().Get("OpEx").Formula; got != "" {
		t.Errorf("OpEx formula = %q, want unchanged empty", got)
	}
	// ...but a dynamic formula on a non-leaf is fine (evaluates post-rollup).
	rec = doJSON(t, h, http.MethodPut, "/api/formulas/OpEx",
		map[string]any{"formula": "A#Sales * 2", "dynamic": true})
	wantStatus(t, rec, http.StatusOK)

	// Same rule through the member-update path.
	f, dyn := "A#Sales - A#COGS", false
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Account/members/BalanceSheet",
		memberUpdate{Formula: &f, DynamicCalc: &dyn})
	wantStatus(t, rec, http.StatusBadRequest)

	// Adding a child under a stored-calc account would un-leaf it: refused.
	rec = doJSON(t, h, http.MethodPost, "/api/dims/Account/members",
		memberCreate{Name: "SubGP", Parent: "GrossProfit"})
	wantStatus(t, rec, http.StatusBadRequest)

	// Moving an existing member under a stored-calc account: refused.
	parent := "GrossProfit"
	rec = doJSON(t, h, http.MethodPut, "/api/dims/Account/members/Rent", memberUpdate{Parent: &parent})
	wantStatus(t, rec, http.StatusBadRequest)
	if got := st.Meta.Account().Get("Rent").Parent; got != "OpEx" {
		t.Errorf("Rent parent = %q, want OpEx", got)
	}
}

// TestMetaLatestDataTime pins the /api/meta hint the UI uses to open on a
// month that actually has data (seed populates through 2026M12).
func TestMetaLatestDataTime(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodGet, "/api/meta", nil)
	wantStatus(t, rec, http.StatusOK)
	got := decodeBody[struct {
		LatestDataTime string `json:"latestDataTime"`
	}](t, rec)
	if got.LatestDataTime != "2026M12" {
		t.Errorf("latestDataTime = %q, want 2026M12", got.LatestDataTime)
	}

	// Empty store: hint is "".
	st := store.NewAppState()
	st.Workflow = workflow.NewRegistry()
	h2 := New(st, t.TempDir()+"/x.json").Handler()
	rec = doJSON(t, h2, http.MethodGet, "/api/meta", nil)
	wantStatus(t, rec, http.StatusOK)
	got = decodeBody[struct {
		LatestDataTime string `json:"latestDataTime"`
	}](t, rec)
	if got.LatestDataTime != "" {
		t.Errorf("latestDataTime = %q, want empty for fresh state", got.LatestDataTime)
	}
}

// TestWriteJSONEncodeFailure pins that an unencodable response body becomes a
// real 500 with a JSON error, not an HTTP 200 with an empty body.
func TestWriteJSONEncodeFailure(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, http.StatusOK, map[string]any{"v": math.Inf(1)})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("body = %q, want JSON error", rec.Body.String())
	}
}

// TestQuerySurfacesDynamicIssues pins that /api/query returns the issues
// recorded for dynamic-calc cycles (SPEC §4 "error recorded").
func TestQuerySurfacesDynamicIssues(t *testing.T) {
	h, _, _ := newTestServer(t)
	rec := doJSON(t, h, http.MethodPut, "/api/formulas/GPMargin",
		map[string]any{"formula": "A#GPMargin + 1", "dynamic": true})
	wantStatus(t, rec, http.StatusOK)

	req := cube.QueryRequest{
		Cube: "GolfTrickle",
		POV:  cube.POV{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M1", Stage: "Local"},
		Rows: []cube.AxisSpec{{Dim: "Account", Member: "GPMargin"}},
		Cols: []cube.AxisSpec{{Dim: "Time", Member: "2025M1"}},
	}
	rec = doJSON(t, h, http.MethodPost, "/api/query", req)
	wantStatus(t, rec, http.StatusOK)
	res := decodeBody[cube.QueryResult](t, rec)
	if !approx(res.Cells[0][0], 0) {
		t.Errorf("self-referential dynamic calc = %v, want 0", res.Cells[0][0])
	}
	if len(res.Issues) != 1 || !strings.Contains(res.Issues[0], "cycle") {
		t.Errorf("issues = %v, want one cycle issue", res.Issues)
	}
}
