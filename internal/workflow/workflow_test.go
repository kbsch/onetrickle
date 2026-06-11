package workflow

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

var (
	t0    = time.Date(2026, 6, 10, 8, 0, 0, 0, time.UTC)
	testK = Key{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M1"}
)

// at returns t0 plus n hours, for distinguishable timestamps.
func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Hour) }

// allStatuses in pipeline order.
var allStatuses = []Status{
	StatusNotStarted, StatusImported, StatusValidated, StatusProcessed, StatusCertified,
}

// pathTo returns the action sequence that drives a fresh unit to status s.
func pathTo(s Status) []string {
	switch s {
	case StatusNotStarted:
		return nil
	case StatusImported:
		return []string{"import"}
	case StatusValidated:
		return []string{"import", "validate"}
	case StatusProcessed:
		return []string{"import", "validate", "process"}
	case StatusCertified:
		return []string{"import", "validate", "process", "certify"}
	}
	return nil
}

// registryAt builds a registry whose testK entry is in status s.
func registryAt(t *testing.T, s Status) *Registry {
	t.Helper()
	r := NewRegistry()
	for i, a := range pathTo(s) {
		if _, err := r.Apply(testK, a, "setup", at(i)); err != nil {
			t.Fatalf("setup Apply(%q) to reach %s: %v", a, s, err)
		}
	}
	if got := r.Status(testK); got != s {
		t.Fatalf("setup reached status %q, want %q", got, s)
	}
	return r
}

func TestKeyKey(t *testing.T) {
	tests := []struct {
		name string
		k    Key
		want string
	}{
		{"full", testK, "GolfTrickle|US Operations|Actual|2025M1"},
		{"empty", Key{}, "|||"},
		{"partial", Key{Cube: "C", Time: "2025M2"}, "C|||2025M2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.k.Key(); got != tt.want {
				t.Errorf("Key() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHappyPath(t *testing.T) {
	r := NewRegistry()
	steps := []struct {
		action   string
		by       string
		wantFrom Status
		wantTo   Status
	}{
		{"import", "alice", StatusNotStarted, StatusImported},
		{"validate", "bob", StatusImported, StatusValidated},
		{"process", "carol", StatusValidated, StatusProcessed},
		{"certify", "dave", StatusProcessed, StatusCertified},
	}
	for i, st := range steps {
		e, err := r.Apply(testK, st.action, st.by, at(i))
		if err != nil {
			t.Fatalf("step %d Apply(%q): %v", i, st.action, err)
		}
		if e.Status != st.wantTo {
			t.Errorf("step %d: status = %q, want %q", i, e.Status, st.wantTo)
		}
		if e.UpdatedBy != st.by {
			t.Errorf("step %d: UpdatedBy = %q, want %q", i, e.UpdatedBy, st.by)
		}
		if !e.UpdatedAt.Equal(at(i)) {
			t.Errorf("step %d: UpdatedAt = %v, want %v", i, e.UpdatedAt, at(i))
		}
		if len(e.History) != i+1 {
			t.Fatalf("step %d: history len = %d, want %d", i, len(e.History), i+1)
		}
		ev := e.History[i]
		want := Event{Action: st.action, From: st.wantFrom, To: st.wantTo, At: at(i), By: st.by}
		if !reflect.DeepEqual(ev, want) {
			t.Errorf("step %d: event = %+v, want %+v", i, ev, want)
		}
		if got := r.Status(testK); got != st.wantTo {
			t.Errorf("step %d: registry Status = %q, want %q", i, got, st.wantTo)
		}
	}
	// The entry is persisted and history is complete and append-only.
	if len(r.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(r.Entries))
	}
	e := r.Get(testK)
	if len(e.History) != 4 {
		t.Errorf("final history len = %d, want 4", len(e.History))
	}
	if e.Key != testK {
		t.Errorf("entry key = %+v, want %+v", e.Key, testK)
	}
}

func TestIllegalTransitions(t *testing.T) {
	legal := map[Status]map[string]bool{
		StatusNotStarted: {"import": true, "reopen": true},
		StatusImported:   {"import": true, "validate": true, "reopen": true},
		StatusValidated:  {"import": true, "process": true, "reopen": true},
		StatusProcessed:  {"import": true, "certify": true, "reopen": true},
		StatusCertified:  {"reopen": true},
	}
	actions := []string{"import", "validate", "process", "certify", "reopen", "bogus", ""}
	for _, s := range allStatuses {
		for _, a := range actions {
			if legal[s][a] {
				continue
			}
			t.Run(string(s)+"/"+a, func(t *testing.T) {
				r := registryAt(t, s)
				before := len(r.Get(testK).History)
				e, err := r.Apply(testK, a, "eve", at(99))
				if err == nil {
					t.Fatalf("Apply(%q) from %q: want error, got entry %+v", a, s, e)
				}
				if e != nil {
					t.Errorf("Apply(%q) from %q: entry = %+v, want nil", a, s, e)
				}
				wantMsg := `action "` + a + `" not allowed from status "` + string(s) + `"`
				if err.Error() != wantMsg {
					t.Errorf("error = %q, want %q", err.Error(), wantMsg)
				}
				// State must be unchanged.
				if got := r.Status(testK); got != s {
					t.Errorf("status after failed Apply = %q, want %q", got, s)
				}
				if after := len(r.Get(testK).History); after != before {
					t.Errorf("history len after failed Apply = %d, want %d", after, before)
				}
			})
		}
	}
}

func TestIllegalActionOnMissingKeyDoesNotStore(t *testing.T) {
	r := NewRegistry()
	_, err := r.Apply(testK, "certify", "eve", t0)
	if err == nil {
		t.Fatal("Apply(certify) on missing key: want error")
	}
	want := `action "certify" not allowed from status "NotStarted"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if len(r.Entries) != 0 {
		t.Errorf("entries = %d after failed Apply, want 0", len(r.Entries))
	}
}

func TestReopenFromEveryStatus(t *testing.T) {
	for _, s := range allStatuses {
		t.Run(string(s), func(t *testing.T) {
			r := registryAt(t, s)
			e, err := r.Apply(testK, "reopen", "ops", at(50))
			if err != nil {
				t.Fatalf("Apply(reopen) from %q: %v", s, err)
			}
			if e.Status != StatusNotStarted {
				t.Errorf("status = %q, want %q", e.Status, StatusNotStarted)
			}
			last := e.History[len(e.History)-1]
			want := Event{Action: "reopen", From: s, To: StatusNotStarted, At: at(50), By: "ops"}
			if !reflect.DeepEqual(last, want) {
				t.Errorf("event = %+v, want %+v", last, want)
			}
			wantLen := len(pathTo(s)) + 1
			if len(e.History) != wantLen {
				t.Errorf("history len = %d, want %d", len(e.History), wantLen)
			}
			// Reopen on an untracked unit stores a NotStarted entry too.
			if _, ok := r.Entries[testK.Key()]; !ok {
				t.Error("entry not persisted after reopen")
			}
		})
	}
}

func TestImportFromEveryNonCertifiedStatus(t *testing.T) {
	for _, s := range allStatuses[:4] { // everything except Certified
		t.Run(string(s), func(t *testing.T) {
			r := registryAt(t, s)
			e, err := r.Apply(testK, "import", "loader", at(60))
			if err != nil {
				t.Fatalf("Apply(import) from %q: %v", s, err)
			}
			if e.Status != StatusImported {
				t.Errorf("status = %q, want %q", e.Status, StatusImported)
			}
			last := e.History[len(e.History)-1]
			if last.From != s || last.To != StatusImported || last.Action != "import" {
				t.Errorf("event = %+v, want import %s->Imported", last, s)
			}
		})
	}
}

func TestImportResetsValidatedToImported(t *testing.T) {
	r := registryAt(t, StatusValidated)
	e, err := r.Apply(testK, "import", "loader", at(10))
	if err != nil {
		t.Fatalf("Apply(import) from Validated: %v", err)
	}
	if e.Status != StatusImported {
		t.Errorf("status = %q, want %q", e.Status, StatusImported)
	}
	if len(e.History) != 3 {
		t.Fatalf("history len = %d, want 3", len(e.History))
	}
	want := Event{Action: "import", From: StatusValidated, To: StatusImported, At: at(10), By: "loader"}
	if !reflect.DeepEqual(e.History[2], want) {
		t.Errorf("event = %+v, want %+v", e.History[2], want)
	}
	// The unit must re-validate before it can process again.
	if _, err := r.Apply(testK, "process", "x", at(11)); err == nil {
		t.Error("process after import-reset: want error, got nil")
	}
}

func TestGetMissingSynthesizesWithoutStoring(t *testing.T) {
	r := NewRegistry()
	e := r.Get(testK)
	if e == nil {
		t.Fatal("Get returned nil")
	}
	if e.Status != StatusNotStarted {
		t.Errorf("synthesized status = %q, want %q", e.Status, StatusNotStarted)
	}
	if e.Key != testK {
		t.Errorf("synthesized key = %+v, want %+v", e.Key, testK)
	}
	if !e.UpdatedAt.IsZero() || e.UpdatedBy != "" || len(e.History) != 0 {
		t.Errorf("synthesized entry not pristine: %+v", e)
	}
	if len(r.Entries) != 0 {
		t.Errorf("entries = %d after Get, want 0 (must not store)", len(r.Entries))
	}
	if got := r.Status(testK); got != StatusNotStarted {
		t.Errorf("Status = %q, want %q", got, StatusNotStarted)
	}
	// Mutating the synthesized entry must not leak into later Gets.
	e.Status = StatusCertified
	if got := r.Get(testK).Status; got != StatusNotStarted {
		t.Errorf("Get after mutating synthesized entry = %q, want %q", got, StatusNotStarted)
	}
}

func TestGetNilSafety(t *testing.T) {
	var r *Registry
	if got := r.Get(testK).Status; got != StatusNotStarted {
		t.Errorf("nil registry Get status = %q, want %q", got, StatusNotStarted)
	}
	if got := r.Status(testK); got != StatusNotStarted {
		t.Errorf("nil registry Status = %q, want %q", got, StatusNotStarted)
	}
	if got := r.List(Key{}); got != nil {
		t.Errorf("nil registry List = %v, want nil", got)
	}
	zero := &Registry{} // nil Entries map
	if got := zero.Status(testK); got != StatusNotStarted {
		t.Errorf("zero registry Status = %q, want %q", got, StatusNotStarted)
	}
	if _, err := zero.Apply(testK, "import", "u", t0); err != nil {
		t.Errorf("Apply on zero registry: %v", err)
	}
	if zero.Status(testK) != StatusImported {
		t.Errorf("zero registry status after import = %q, want Imported", zero.Status(testK))
	}
}

func TestActions(t *testing.T) {
	tests := []struct {
		s    Status
		want []string
	}{
		{StatusNotStarted, []string{"import", "reopen"}},
		{StatusImported, []string{"import", "validate", "reopen"}},
		{StatusValidated, []string{"import", "process", "reopen"}},
		{StatusProcessed, []string{"import", "certify", "reopen"}},
		{StatusCertified, []string{"reopen"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.s), func(t *testing.T) {
			if got := Actions(tt.s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Actions(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestAllowed(t *testing.T) {
	tests := []struct {
		s      Status
		action string
		want   bool
	}{
		{StatusNotStarted, ActionImport, true},
		{StatusNotStarted, ActionValidate, false},
		{StatusNotStarted, ActionProcess, false},
		{StatusImported, ActionValidate, true},
		{StatusValidated, ActionProcess, true},
		{StatusProcessed, ActionCertify, true},
		{StatusCertified, ActionImport, false},
		{StatusCertified, ActionReopen, true},
		{StatusValidated, "bogus", false},
	}
	for _, tt := range tests {
		if got := Allowed(tt.s, tt.action); got != tt.want {
			t.Errorf("Allowed(%q, %q) = %v, want %v", tt.s, tt.action, got, tt.want)
		}
	}
}

func TestListFilteringAndOrdering(t *testing.T) {
	r := NewRegistry()
	keys := []Key{
		{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M2"},
		{Cube: "GolfTrickle", Entity: "Canada", Scenario: "Actual", Time: "2025M1"},
		{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Budget", Time: "2025M1"},
		{Cube: "Other", Entity: "Canada", Scenario: "Actual", Time: "2025M1"},
		{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Actual", Time: "2025M1"},
	}
	for i, k := range keys {
		if _, err := r.Apply(k, "import", "u", at(i)); err != nil {
			t.Fatalf("setup Apply(%v): %v", k, err)
		}
	}
	tests := []struct {
		name   string
		filter Key
		want   []string // expected Key.Key()s, in order
	}{
		{
			"all",
			Key{},
			[]string{
				"GolfTrickle|Canada|Actual|2025M1",
				"GolfTrickle|US Operations|Actual|2025M1",
				"GolfTrickle|US Operations|Actual|2025M2",
				"GolfTrickle|US Operations|Budget|2025M1",
				"Other|Canada|Actual|2025M1",
			},
		},
		{
			"by cube",
			Key{Cube: "GolfTrickle"},
			[]string{
				"GolfTrickle|Canada|Actual|2025M1",
				"GolfTrickle|US Operations|Actual|2025M1",
				"GolfTrickle|US Operations|Actual|2025M2",
				"GolfTrickle|US Operations|Budget|2025M1",
			},
		},
		{
			"by entity",
			Key{Entity: "Canada"},
			[]string{
				"GolfTrickle|Canada|Actual|2025M1",
				"Other|Canada|Actual|2025M1",
			},
		},
		{
			"cube+scenario+time",
			Key{Cube: "GolfTrickle", Scenario: "Actual", Time: "2025M1"},
			[]string{
				"GolfTrickle|Canada|Actual|2025M1",
				"GolfTrickle|US Operations|Actual|2025M1",
			},
		},
		{
			"exact",
			Key{Cube: "GolfTrickle", Entity: "US Operations", Scenario: "Budget", Time: "2025M1"},
			[]string{"GolfTrickle|US Operations|Budget|2025M1"},
		},
		{"no match", Key{Cube: "Nope"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := r.List(tt.filter)
			got := make([]string, 0, len(entries))
			for _, e := range entries {
				got = append(got, e.Key.Key())
			}
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List(%+v) keys = %v, want %v", tt.filter, got, tt.want)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	r := NewRegistry()
	k2 := Key{Cube: "GolfTrickle", Entity: "Germany", Scenario: "Budget", Time: "2025M3"}
	for i, a := range []string{"import", "validate", "process", "certify"} {
		if _, err := r.Apply(testK, a, "alice", at(i)); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	if _, err := r.Apply(k2, "import", "bob", at(7)); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if _, err := r.Apply(k2, "reopen", "bob", at(8)); err != nil {
		t.Fatalf("setup: %v", err)
	}

	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Shape check: top level is exactly the {"<key>": entry} map.
	var shape map[string]map[string]json.RawMessage
	if err := json.Unmarshal(b, &shape); err != nil {
		t.Fatalf("registry JSON is not a map of objects: %v", err)
	}
	if len(shape) != 2 {
		t.Fatalf("marshaled registry has %d keys, want 2", len(shape))
	}
	ent, ok := shape[testK.Key()]
	if !ok {
		t.Fatalf("marshaled registry missing key %q; keys: %v", testK.Key(), shape)
	}
	for _, field := range []string{"key", "status", "updatedAt", "updatedBy", "history"} {
		if _, ok := ent[field]; !ok {
			t.Errorf("entry JSON missing field %q", field)
		}
	}

	var rt Registry
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(rt.Entries, r.Entries) {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", rt.Entries, r.Entries)
	}
	if got := rt.Status(testK); got != StatusCertified {
		t.Errorf("round-trip Status(testK) = %q, want %q", got, StatusCertified)
	}
	if got := rt.Status(k2); got != StatusNotStarted {
		t.Errorf("round-trip Status(k2) = %q, want %q", got, StatusNotStarted)
	}
	if got := len(rt.Get(testK).History); got != 4 {
		t.Errorf("round-trip history len = %d, want 4", got)
	}

	// Empty and nil-entries registries marshal as {} and round-trip.
	for name, reg := range map[string]*Registry{"empty": NewRegistry(), "zero": {}} {
		b, err := json.Marshal(reg)
		if err != nil {
			t.Fatalf("%s Marshal: %v", name, err)
		}
		if string(b) != "{}" {
			t.Errorf("%s registry JSON = %s, want {}", name, b)
		}
	}
	var fresh Registry
	if err := json.Unmarshal([]byte("{}"), &fresh); err != nil {
		t.Fatalf("Unmarshal {}: %v", err)
	}
	if fresh.Entries == nil || len(fresh.Entries) != 0 {
		t.Errorf("Unmarshal {} → Entries = %v, want empty non-nil map", fresh.Entries)
	}
}

func TestJSONEventFieldNames(t *testing.T) {
	ev := Event{Action: "import", From: StatusNotStarted, To: StatusImported, At: t0, By: "alice"}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, field := range []string{"action", "from", "to", "at", "by"} {
		if _, ok := m[field]; !ok {
			t.Errorf("event JSON missing field %q; got %s", field, b)
		}
	}
	kb, err := json.Marshal(testK)
	if err != nil {
		t.Fatalf("Marshal key: %v", err)
	}
	var km map[string]json.RawMessage
	if err := json.Unmarshal(kb, &km); err != nil {
		t.Fatalf("Unmarshal key: %v", err)
	}
	for _, field := range []string{"cube", "entity", "scenario", "time"} {
		if _, ok := km[field]; !ok {
			t.Errorf("key JSON missing field %q; got %s", field, kb)
		}
	}
}

func TestUnmarshalBadKey(t *testing.T) {
	var r Registry
	err := json.Unmarshal([]byte(`{"only|three|parts":{"status":"Imported"}}`), &r)
	if err == nil {
		t.Fatal("Unmarshal with bad key: want error")
	}
}
