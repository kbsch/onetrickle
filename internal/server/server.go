// Package server implements the onetrickle HTTP API (SPEC §10) and serves the
// embedded web UI. One RWMutex serializes all access to the AppState: read
// endpoints take the read lock, mutating endpoints take the write lock and
// persist the JSON snapshot before replying (a failed save is a 500; the
// in-memory state stays mutated).
package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"onetrickle/internal/calc"
	"onetrickle/internal/cube"
	"onetrickle/internal/model"
	"onetrickle/internal/store"
	"onetrickle/web"
)

// Server holds the application state, the wired query engine and the snapshot
// path. All handler access to state goes through mu.
type Server struct {
	mu       sync.RWMutex
	state    *store.AppState
	engine   *cube.Engine
	dataPath string
}

// New builds a server over state, persisting snapshots to dataPath. The cube
// engine's dynamic-calc evaluator is wired to the calc package.
func New(state *store.AppState, dataPath string) *Server {
	eng := cube.NewEngine(state.Cells)
	eng.DynEval = func(meta *model.Metadata, pov cube.POV, formula string, getRef func(account string) float64) (float64, error) {
		expr, err := calc.Parse(formula)
		if err != nil {
			return 0, err
		}
		return expr.Eval(func(a string) (float64, error) { return getRef(a), nil })
	}
	return &Server{state: state, engine: eng, dataPath: dataPath}
}

// Handler returns the full route table: the JSON API under /api/ and the
// embedded static UI at /. Every request is logged.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/meta", s.handleMeta)

	mux.HandleFunc("GET /api/dims/{type}/members", s.handleDimMembers)
	mux.HandleFunc("POST /api/dims/{type}/members", s.handleDimAdd)
	mux.HandleFunc("PUT /api/dims/{type}/members/{name}", s.handleDimUpdate)
	mux.HandleFunc("DELETE /api/dims/{type}/members/{name}", s.handleDimDelete)

	mux.HandleFunc("GET /api/rates", s.handleRatesGet)
	mux.HandleFunc("PUT /api/rates", s.handleRatesPut)

	mux.HandleFunc("POST /api/data/cells", s.handleDataCells)
	mux.HandleFunc("POST /api/query", s.handleQuery)
	mux.HandleFunc("GET /api/export", s.handleExport)
	mux.HandleFunc("POST /api/process", s.handleProcess)

	mux.HandleFunc("GET /api/profiles", s.handleProfilesGet)
	mux.HandleFunc("POST /api/profiles", s.handleProfilesPost)
	mux.HandleFunc("PUT /api/profiles/{name}", s.handleProfilePut)
	mux.HandleFunc("DELETE /api/profiles/{name}", s.handleProfileDelete)
	mux.HandleFunc("POST /api/import/preview", s.handleImportPreview)
	mux.HandleFunc("POST /api/import/commit", s.handleImportCommit)

	mux.HandleFunc("GET /api/workflow", s.handleWorkflowGet)
	mux.HandleFunc("POST /api/workflow/action", s.handleWorkflowAction)

	mux.HandleFunc("GET /api/formulas", s.handleFormulasGet)
	mux.HandleFunc("PUT /api/formulas/{account}", s.handleFormulaPut)

	// Unknown /api/* paths answer with a JSON 404, not the SPA shell.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, fmt.Errorf("unknown API endpoint %s %s", r.Method, r.URL.Path))
	})

	// Static UI from the embedded filesystem. "/" serves the SPA shell for
	// every non-API GET (hash routing handles the rest client-side). The root
	// pattern is method-less because a "GET /" pattern would conflict with the
	// method-less "/api/" catch-all in the 1.22 mux; the method is checked in
	// the handler instead.
	mux.HandleFunc("GET /app.js", serveStatic("app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET /style.css", serveStatic("style.css", "text/css; charset=utf-8"))
	index := serveStatic("index.html", "text/html; charset=utf-8")
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method %s not allowed on %s", r.Method, r.URL.Path))
			return
		}
		index(w, r)
	})

	return logMiddleware(mux)
}

// saveLocked persists the current state snapshot. The caller must hold the
// write lock.
func (s *Server) saveLocked() error {
	if err := store.Save(s.dataPath, s.state); err != nil {
		return fmt.Errorf("persist state: %w", err)
	}
	return nil
}

// writeJSON writes v as a JSON response with the given status code. The body
// is marshaled BEFORE the status is written so an encoding failure becomes a
// proper 500 with a JSON error instead of a truncated 200.
func writeJSON(w http.ResponseWriter, code int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("server: encode response: %v", err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"encode response failed"}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(append(data, '\n'))
}

// writeError writes {"error": msg} with the given status code.
func writeError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// decodeJSON decodes the request body into v.
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

// serveStatic serves one embedded asset with a fixed content type.
func serveStatic(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := web.FS.ReadFile(name)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("read embedded asset %s: %w", name, err))
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	}
}

// statusWriter records the response status for the request log.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logMiddleware logs method, path, status and duration of every request.
func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start))
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
