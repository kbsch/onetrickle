package server

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"onetrickle/internal/consol"
	"onetrickle/internal/model"
	"onetrickle/internal/workflow"
)

// workflowEntryDTO is one unit's workflow state on the wire. Entity is
// duplicated at the top level for the UI's entity-keyed board.
type workflowEntryDTO struct {
	Key       workflow.Key    `json:"key"`
	Entity    string          `json:"entity"`
	Status    workflow.Status `json:"status"`
	UpdatedAt time.Time       `json:"updatedAt,omitzero"`
	UpdatedBy string          `json:"updatedBy,omitempty"`
	IsLeaf    bool            `json:"isLeaf"`
}

func (s *Server) handleWorkflowGet(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	cubeName, scenario, timeMonth := q.Get("cube"), q.Get("scenario"), q.Get("time")
	if cubeName == "" || scenario == "" || timeMonth == "" {
		writeError(w, http.StatusBadRequest, errors.New("cube, scenario and time query parameters are required"))
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.state.Meta.Entity()
	out := []workflowEntryDTO{}
	var walk func(name string)
	walk = func(name string) {
		m := d.Get(name)
		if m == nil {
			return
		}
		k := workflow.Key{Cube: cubeName, Entity: name, Scenario: scenario, Time: timeMonth}
		e := s.state.Workflow.Get(k) // synthesizes NotStarted when untracked
		out = append(out, workflowEntryDTO{
			Key:       k,
			Entity:    name,
			Status:    e.Status,
			UpdatedAt: e.UpdatedAt,
			UpdatedBy: e.UpdatedBy,
			IsLeaf:    len(m.Children) == 0,
		})
		for _, c := range m.Children {
			walk(c)
		}
	}
	for _, root := range d.Roots {
		walk(root)
	}
	writeJSON(w, http.StatusOK, out)
}

// workflowActionReq is the POST /api/workflow/action request body.
type workflowActionReq struct {
	Cube     string `json:"cube"`
	Entity   string `json:"entity"`
	Scenario string `json:"scenario"`
	Time     string `json:"time"`
	Action   string `json:"action"`
	By       string `json:"by"`
}

func (s *Server) handleWorkflowAction(w http.ResponseWriter, r *http.Request) {
	var req workflowActionReq
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	meta := s.state.Meta
	if _, err := meta.CubeOf(req.Cube); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if !meta.Entity().Has(req.Entity) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown entity %q", req.Entity))
		return
	}
	if d := meta.Dim(model.DimScenario); d == nil || !d.Has(req.Scenario) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("unknown scenario %q", req.Scenario))
		return
	}
	if !model.TimeIsMonth(req.Time) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("time %q is not a month member", req.Time))
		return
	}
	if req.Action == "" {
		writeError(w, http.StatusBadRequest, errors.New("action is required"))
		return
	}
	by := req.By
	if by == "" {
		by = "admin"
	}

	// Validate the transition BEFORE any side effect: a rejected action must
	// not mutate state (in particular "process" must not run a full-cube
	// consolidation that is never persisted).
	k := workflow.Key{Cube: req.Cube, Entity: req.Entity, Scenario: req.Scenario, Time: req.Time}
	if st := s.state.Workflow.Status(k); !workflow.Allowed(st, req.Action) {
		writeError(w, http.StatusConflict, fmt.Errorf("action %q not allowed from status %q", req.Action, st))
		return
	}

	// "process" runs the full-cube consolidation for the slice; its data
	// issues are returned alongside the workflow entry.
	issues := []string{}
	if req.Action == workflow.ActionProcess {
		res, err := consol.Process(meta, s.state.Cells, req.Cube, req.Scenario, req.Time)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("process: %w", err))
			return
		}
		issues = res.Issues
	}

	entry, err := s.state.Workflow.Apply(k, req.Action, by, time.Now().UTC())
	if err != nil {
		// Unreachable after the Allowed pre-check; defensive.
		writeError(w, http.StatusConflict, err)
		return
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry, "issues": issues})
}
