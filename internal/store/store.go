// Package store persists the whole application state as a single JSON
// snapshot file (SPEC §9). The snapshot bundles the dimensional metadata,
// the cube cell store, the import profiles and the workflow registry:
//
//	{"version":1,"metadata":...,"units":...,"profiles":...,"workflow":...}
//
// Save writes atomically (temp file + rename); Load tolerates a missing file
// (fresh state) and defensively re-initializes nil sub-structures so old or
// hand-edited snapshots cannot panic the server.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/stage"
	"onetrickle/internal/workflow"
)

// snapshotVersion is the only snapshot format version this build reads and
// writes. Any other version on disk is an error (no silent migrations).
const snapshotVersion = 1

// AppState bundles everything the server persists.
type AppState struct {
	Meta     *model.Metadata
	Cells    *cube.Store
	Profiles map[string]*stage.Profile
	Workflow *workflow.Registry
}

// NewAppState returns a fully initialized empty state: structurally valid
// metadata, an empty cell store, no import profiles and an empty workflow
// registry.
func NewAppState() *AppState {
	return &AppState{
		Meta:     model.NewMetadata(),
		Cells:    cube.NewStore(),
		Profiles: map[string]*stage.Profile{},
		Workflow: workflow.NewRegistry(),
	}
}

// snapshot is the on-disk JSON shape of an AppState.
type snapshot struct {
	Version  int                       `json:"version"`
	Metadata *model.Metadata           `json:"metadata"`
	Units    *cube.Store               `json:"units"`
	Profiles map[string]*stage.Profile `json:"profiles"`
	Workflow *workflow.Registry        `json:"workflow"`
}

// Save writes s as a JSON snapshot to path, atomically: the snapshot is
// written to <path>.tmp (mode 0644) and then renamed over path. The parent
// directory of path is created if missing.
func Save(path string, s *AppState) error {
	if s == nil {
		return fmt.Errorf("store: save %s: nil state", path)
	}
	snap := snapshot{
		Version:  snapshotVersion,
		Metadata: s.Meta,
		Units:    s.Cells,
		Profiles: s.Profiles,
		Workflow: s.Workflow,
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("store: save %s: marshal snapshot: %w", path, err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("store: save %s: create data dir: %w", path, err)
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("store: save %s: write temp file: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best effort: do not leave the temp file behind
		return fmt.Errorf("store: save %s: rename temp file: %w", path, err)
	}
	return nil
}

// Load reads the snapshot at path. A missing file yields a fresh NewAppState
// (nil error). Corrupted JSON or an unknown snapshot version is an error.
// After decoding, nil sub-structures are re-initialized (see normalize).
func Load(path string) (*AppState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewAppState(), nil
		}
		return nil, fmt.Errorf("store: load %s: %w", path, err)
	}
	// Probe the version first so a future-format snapshot reports a clear
	// version error instead of a shape mismatch.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("store: load %s: parse snapshot: %w", path, err)
	}
	if probe.Version != snapshotVersion {
		return nil, fmt.Errorf("store: load %s: unsupported snapshot version %d (want %d)", path, probe.Version, snapshotVersion)
	}
	var snap snapshot
	if err := decodeSnapshot(data, &snap); err != nil {
		return nil, fmt.Errorf("store: load %s: %w", path, err)
	}
	s := &AppState{
		Meta:     snap.Metadata,
		Cells:    snap.Units,
		Profiles: snap.Profiles,
		Workflow: snap.Workflow,
	}
	normalize(s)
	return s, nil
}

// decodeSnapshot unmarshals a snapshot, converting any panic raised by a
// custom UnmarshalJSON on malformed hand-edited input into an error so Load
// never panics the server.
func decodeSnapshot(data []byte, snap *snapshot) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("parse snapshot: invalid contents: %v", p)
		}
	}()
	if err := json.Unmarshal(data, snap); err != nil {
		return fmt.Errorf("parse snapshot: %w", err)
	}
	return nil
}

// normalize re-initializes any nil sub-structure of a freshly loaded state so
// old or hand-edited snapshots cannot panic the server: nil metadata maps and
// dimensions, nil cell store / unit maps, nil profile map and nil workflow
// registry all become valid empty values. Nil map entries are dropped.
func normalize(s *AppState) {
	if s.Meta == nil {
		s.Meta = model.NewMetadata()
	}
	m := s.Meta
	if m.Cubes == nil {
		m.Cubes = map[string]*model.Cube{}
	}
	for name, c := range m.Cubes {
		if c == nil {
			delete(m.Cubes, name)
		}
	}
	if m.Dims == nil {
		m.Dims = map[model.DimType]*model.Dimension{}
	}
	for t, d := range m.Dims {
		if d == nil {
			delete(m.Dims, t)
		}
	}
	for _, t := range model.AllDims {
		d := m.Dims[t]
		if d == nil {
			d = model.NewDimension(t)
			m.Dims[t] = d
		}
		if d.Type == "" {
			d.Type = t
		}
		if d.Members == nil {
			d.Members = map[string]*model.Member{}
		}
		for name, mem := range d.Members {
			if mem == nil {
				delete(d.Members, name)
			}
		}
	}
	if m.Rates == nil {
		m.Rates = model.RateTable{}
	}

	if s.Cells == nil {
		s.Cells = cube.NewStore()
	}
	if s.Cells.Units == nil {
		s.Cells.Units = map[cube.UnitKey]*cube.Unit{}
	}
	for k, u := range s.Cells.Units {
		if u == nil {
			delete(s.Cells.Units, k)
			continue
		}
		if u.Input == nil {
			u.Input = cube.CellMap{}
		}
	}

	if s.Profiles == nil {
		s.Profiles = map[string]*stage.Profile{}
	}
	for name, p := range s.Profiles {
		if p == nil {
			delete(s.Profiles, name)
		}
	}

	if s.Workflow == nil {
		s.Workflow = workflow.NewRegistry()
	}
	if s.Workflow.Entries == nil {
		s.Workflow.Entries = map[string]*workflow.Entry{}
	}
}
