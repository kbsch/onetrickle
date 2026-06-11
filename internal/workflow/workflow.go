// Package workflow tracks the workflow state of data units (SPEC §8).
//
// Each data unit, addressed by (Cube, Entity, Scenario, Time-month), moves
// through the states NotStarted → Imported → Validated → Processed →
// Certified via the actions import, validate, process, certify and reopen.
// A Registry stores one Entry per touched unit; units never touched are
// implicitly NotStarted.
package workflow

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is the workflow state of one data unit.
type Status string

// Workflow states, in pipeline order.
const (
	StatusNotStarted Status = "NotStarted"
	StatusImported   Status = "Imported"
	StatusValidated  Status = "Validated"
	StatusProcessed  Status = "Processed"
	StatusCertified  Status = "Certified"
)

// Workflow action names accepted by Registry.Apply.
const (
	ActionImport   = "import"
	ActionValidate = "validate"
	ActionProcess  = "process"
	ActionCertify  = "certify"
	ActionReopen   = "reopen"
)

// Key addresses one data unit. Time is always a month member.
type Key struct {
	Cube     string `json:"cube"`
	Entity   string `json:"entity"`
	Scenario string `json:"scenario"`
	Time     string `json:"time"`
}

// Key encodes the key as "cube|entity|scenario|time".
func (k Key) Key() string {
	return strings.Join([]string{k.Cube, k.Entity, k.Scenario, k.Time}, "|")
}

// parseKey reverses Key.Key.
func parseKey(s string) (Key, error) {
	p := strings.Split(s, "|")
	if len(p) != 4 {
		return Key{}, fmt.Errorf("bad workflow key %q", s)
	}
	return Key{Cube: p[0], Entity: p[1], Scenario: p[2], Time: p[3]}, nil
}

// Event is one append-only history record of an applied action.
type Event struct {
	Action string    `json:"action"`
	From   Status    `json:"from"`
	To     Status    `json:"to"`
	At     time.Time `json:"at"`
	By     string    `json:"by"`
}

// Entry is the workflow record of one data unit.
type Entry struct {
	Key       Key       `json:"key"`
	Status    Status    `json:"status"`
	UpdatedAt time.Time `json:"updatedAt"`
	UpdatedBy string    `json:"updatedBy"`
	History   []Event   `json:"history"`
}

// Registry holds the workflow entries of all touched data units, keyed by
// Key.Key(). It JSON-marshals as that map: {"<key>": entry}.
type Registry struct {
	Entries map[string]*Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{Entries: map[string]*Entry{}}
}

// Get returns the entry for k. Missing units yield a synthesized NotStarted
// entry that is NOT stored in the registry; the result is never nil.
func (r *Registry) Get(k Key) *Entry {
	if r != nil && r.Entries != nil {
		if e := r.Entries[k.Key()]; e != nil {
			return e
		}
	}
	return &Entry{Key: k, Status: StatusNotStarted}
}

// Status returns the current status of k (NotStarted when untracked).
func (r *Registry) Status(k Key) Status {
	return r.Get(k).Status
}

// transition returns the status reached by applying action from s,
// or ok=false when the action is unknown or not allowed from s.
func transition(s Status, action string) (Status, bool) {
	switch action {
	case ActionImport:
		if s != StatusCertified {
			return StatusImported, true
		}
	case ActionValidate:
		if s == StatusImported {
			return StatusValidated, true
		}
	case ActionProcess:
		if s == StatusValidated {
			return StatusProcessed, true
		}
	case ActionCertify:
		if s == StatusProcessed {
			return StatusCertified, true
		}
	case ActionReopen:
		return StatusNotStarted, true
	}
	return "", false
}

// Allowed reports whether action is a legal transition from status s.
func Allowed(s Status, action string) bool {
	_, ok := transition(s, action)
	return ok
}

// Actions returns the legal actions from a status, in pipeline order
// (import, validate, process, certify, reopen).
func Actions(s Status) []string {
	order := [...]string{ActionImport, ActionValidate, ActionProcess, ActionCertify, ActionReopen}
	var out []string
	for _, a := range order {
		if _, ok := transition(s, a); ok {
			out = append(out, a)
		}
	}
	return out
}

// Apply performs action on the unit k as user by at time at. On success it
// appends an Event to the entry's history, updates Status/UpdatedAt/UpdatedBy,
// persists the entry in the registry (creating it if the unit was untracked)
// and returns it. Illegal or unknown actions return an error and leave the
// registry unchanged.
func (r *Registry) Apply(k Key, action, by string, at time.Time) (*Entry, error) {
	e := r.Get(k)
	next, ok := transition(e.Status, action)
	if !ok {
		return nil, fmt.Errorf("action %q not allowed from status %q", action, e.Status)
	}
	e.History = append(e.History, Event{Action: action, From: e.Status, To: next, At: at, By: by})
	e.Status = next
	e.UpdatedAt = at
	e.UpdatedBy = by
	if r.Entries == nil {
		r.Entries = map[string]*Entry{}
	}
	r.Entries[k.Key()] = e
	return e, nil
}

// List returns the stored entries matching filter (empty fields match all),
// sorted by Key.Key(). Synthesized NotStarted entries are not included.
func (r *Registry) List(filter Key) []*Entry {
	if r == nil {
		return nil
	}
	keys := make([]string, 0, len(r.Entries))
	for ks, e := range r.Entries {
		if e == nil {
			continue
		}
		if (filter.Cube == "" || e.Key.Cube == filter.Cube) &&
			(filter.Entity == "" || e.Key.Entity == filter.Entity) &&
			(filter.Scenario == "" || e.Key.Scenario == filter.Scenario) &&
			(filter.Time == "" || e.Key.Time == filter.Time) {
			keys = append(keys, ks)
		}
	}
	sort.Strings(keys)
	out := make([]*Entry, 0, len(keys))
	for _, ks := range keys {
		out = append(out, r.Entries[ks])
	}
	return out
}

// MarshalJSON encodes the registry as {"<key>": entry}.
func (r *Registry) MarshalJSON() ([]byte, error) {
	m := r.Entries
	if m == nil {
		m = map[string]*Entry{}
	}
	return json.Marshal(m)
}

// UnmarshalJSON reverses MarshalJSON. Entry keys are re-derived from the map
// keys so the Entries invariant (map key == Entry.Key.Key()) always holds.
func (r *Registry) UnmarshalJSON(b []byte) error {
	var raw map[string]*Entry
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("unmarshal workflow registry: %w", err)
	}
	entries := make(map[string]*Entry, len(raw))
	for ks, e := range raw {
		if e == nil {
			continue
		}
		k, err := parseKey(ks)
		if err != nil {
			return fmt.Errorf("unmarshal workflow registry: %w", err)
		}
		e.Key = k
		if e.Status == "" {
			e.Status = StatusNotStarted
		}
		entries[ks] = e
	}
	r.Entries = entries
	return nil
}
