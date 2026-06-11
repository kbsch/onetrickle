package server

import (
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"time"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/stage"
	"onetrickle/internal/workflow"
)

// maxImportMemory bounds the in-memory part of multipart import uploads.
const maxImportMemory = 32 << 20

// maxImportUpload caps the total import request body (http.MaxBytesReader);
// larger uploads are rejected with 413 before anything is parsed.
const maxImportUpload = 20 << 20

// maxImportRows caps the number of CSV rows one import may carry; more rows
// are rejected with 413 (each retained row costs memory during transform).
const maxImportRows = 200_000

// errImportTooLarge marks oversized import uploads (mapped to 413).
var errImportTooLarge = errors.New("import too large")

func (s *Server) handleProfilesGet(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.state.Profiles))
	for name := range s.state.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	out := []*stage.Profile{}
	for _, name := range names {
		out = append(out, s.state.Profiles[name])
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleProfilesPost(w http.ResponseWriter, r *http.Request) {
	var p stage.Profile
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if p.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("profile name is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Profiles[p.Name]; exists {
		writeError(w, http.StatusConflict, fmt.Errorf("profile %q already exists", p.Name))
		return
	}
	s.state.Profiles[p.Name] = &p
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, &p)
}

func (s *Server) handleProfilePut(w http.ResponseWriter, r *http.Request) {
	var p stage.Profile
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	p.Name = r.PathValue("name") // the path is authoritative
	if p.Name == "" {
		writeError(w, http.StatusBadRequest, errors.New("profile name is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.Profiles[p.Name] = &p
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, &p)
}

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.Profiles[name]; !exists {
		writeError(w, http.StatusNotFound, fmt.Errorf("profile %q not found", name))
		return
	}
	delete(s.state.Profiles, name)
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// readImportForm extracts the profile name and uploaded CSV bytes from a
// multipart import request (form fields "profile" and "file"). The request
// body is capped at maxImportUpload; overflow yields errImportTooLarge.
func readImportForm(w http.ResponseWriter, r *http.Request) (profile string, data []byte, err error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportUpload)
	if err := r.ParseMultipartForm(maxImportMemory); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return "", nil, fmt.Errorf("%w: upload exceeds the %d MB limit", errImportTooLarge, maxImportUpload>>20)
		}
		return "", nil, fmt.Errorf("parse multipart form: %w", err)
	}
	profile = r.FormValue("profile")
	if profile == "" {
		return "", nil, errors.New(`missing form field "profile"`)
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		return "", nil, fmt.Errorf(`missing form file "file": %w`, err)
	}
	defer f.Close()
	data, err = io.ReadAll(f)
	if err != nil {
		return "", nil, fmt.Errorf("read uploaded file: %w", err)
	}
	return profile, data, nil
}

// importErrorCode maps an import pipeline error to its HTTP status.
func importErrorCode(err error) int {
	if errors.Is(err, errImportTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

// transformImport runs stage.Transform with the row cap applied.
func transformImport(p *stage.Profile, data []byte) (*stage.TransformResult, error) {
	res, err := stage.Transform(p, data)
	if err != nil {
		return nil, err
	}
	if len(res.Rows) > maxImportRows {
		return nil, fmt.Errorf("%w: file has %d rows (limit %d)", errImportTooLarge, len(res.Rows), maxImportRows)
	}
	return res, nil
}

// previewRowLimit caps the rows echoed back by /api/import/preview.
const previewRowLimit = 200

func (s *Server) handleImportPreview(w http.ResponseWriter, r *http.Request) {
	name, data, err := readImportForm(w, r)
	if err != nil {
		writeError(w, importErrorCode(err), err)
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p := s.state.Profiles[name]
	if p == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("profile %q not found", name))
		return
	}
	res, err := transformImport(p, data)
	if err != nil {
		writeError(w, importErrorCode(err), err)
		return
	}
	stage.Validate(s.state.Meta, res)

	rows := res.Rows
	if len(rows) > previewRowLimit {
		rows = rows[:previewRowLimit]
	}
	clean := 0
	for _, row := range res.Rows {
		if row != nil && len(row.Issues) == 0 {
			clean++
		}
	}
	issues := res.Issues
	if issues == nil {
		issues = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":      rows,
		"issues":    issues,
		"totalRows": len(res.Rows),
		"cleanRows": clean,
	})
}

func (s *Server) handleImportCommit(w http.ResponseWriter, r *http.Request) {
	name, data, err := readImportForm(w, r)
	if err != nil {
		writeError(w, importErrorCode(err), err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.state.Profiles[name]
	if p == nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("profile %q not found", name))
		return
	}
	res, err := transformImport(p, data)
	if err != nil {
		writeError(w, importErrorCode(err), err)
		return
	}
	stage.Validate(s.state.Meta, res)
	plan := stage.LoadPlan(res)

	keys := make([]cube.UnitKey, 0, len(plan))
	for k := range plan {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key() < keys[j].Key() })

	// Refuse the whole commit when any target unit is certified, and when any
	// aggregated amount is non-finite (finite per-row amounts can still sum to
	// ±Inf; such a value would poison JSON persistence forever).
	for _, k := range keys {
		if s.state.Workflow.Status(wfKey(k)) == workflow.StatusCertified {
			writeError(w, http.StatusConflict, fmt.Errorf("unit %s: %w", k.Key(), errCertified))
			return
		}
		for _, wr := range plan[k] {
			if math.IsNaN(wr.Value) || math.IsInf(wr.Value, 0) {
				writeError(w, http.StatusBadRequest,
					fmt.Errorf("unit %s, coord %s: summed amount is not finite", k.Key(), wr.Coord.Key()))
				return
			}
		}
	}

	// Replace semantics: clear Origin=Import on each unit in the plan, then
	// apply its writes (value 0 deletes), then move the unit's workflow.
	cellsWritten := 0
	now := time.Now().UTC()
	for _, k := range keys {
		u := s.state.Cells.Ensure(k)
		u.ClearOrigin(model.OriginImport)
		for _, wr := range plan[k] {
			if wr.Value == 0 {
				delete(u.Input, wr.Coord)
			} else {
				u.Input[wr.Coord] = wr.Value
			}
			cellsWritten++
		}
		if _, err := s.state.Workflow.Apply(wfKey(k), workflow.ActionImport, "admin", now); err != nil {
			// Unreachable after the certified pre-check; defensive.
			writeError(w, http.StatusInternalServerError, fmt.Errorf("workflow import for %s: %w", k.Key(), err))
			return
		}
	}
	skipped := 0
	for _, row := range res.Rows {
		if row == nil || len(row.Issues) > 0 {
			skipped++
		}
	}
	if err := s.saveLocked(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	issues := res.Issues
	if issues == nil {
		issues = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"unitsLoaded":  len(keys),
		"cellsWritten": cellsWritten,
		"skippedRows":  skipped,
		"issues":       issues,
	})
}
