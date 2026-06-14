// Command onetrickle runs the onetrickle CPM server, or seeds the GolfTrickle
// demo dataset:
//
//	onetrickle -data ./data -addr :8080
//	onetrickle seed -data ./data
//
// The seed subcommand refuses to overwrite an existing snapshot.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"onetrickle/internal/consol"
	"onetrickle/internal/seed"
	"onetrickle/internal/server"
	"onetrickle/internal/store"
	"onetrickle/internal/workflow"
)

// snapshotFile is the snapshot's file name inside the -data directory.
const snapshotFile = "onetrickle.json"

func main() {
	args := os.Args[1:]
	isSeed := len(args) > 0 && args[0] == "seed"
	if isSeed {
		args = args[1:]
	}
	fs := flag.NewFlagSet("onetrickle", flag.ExitOnError)
	dataDir := fs.String("data", "./data", "data directory for the JSON snapshot")
	addr := fs.String("addr", ":8080", "HTTP listen address")
	_ = fs.Parse(args)
	path := filepath.Join(*dataDir, snapshotFile)

	var err error
	if isSeed {
		err = runSeed(path)
	} else {
		err = runServe(path, *addr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "onetrickle:", err)
		os.Exit(1)
	}
}

// runSeed builds the GolfTrickle demo, consolidates every seeded (scenario,
// month) slice, walks each leaf entity's workflow to Processed for all of
// them and saves the snapshot. No unit is certified. It refuses to overwrite
// an existing snapshot.
func runSeed(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot %s already exists; refusing to overwrite (delete it or use another -data directory)", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	meta, cells, profiles, err := seed.Build()
	if err != nil {
		return fmt.Errorf("build seed data: %w", err)
	}
	st := store.NewAppState()
	st.Meta, st.Cells, st.Profiles = meta, cells, profiles

	var leaves []string
	for _, root := range meta.Entity().Roots {
		leaves = append(leaves, meta.Entity().Leaves(root)...)
	}

	now := time.Now().UTC()
	totalCells, totalIssues, processed := 0, 0, 0
	for _, slice := range seed.Slices() {
		res, err := consol.Process(meta, cells, seed.CubeName, slice.Scenario, slice.Time)
		if err != nil {
			return fmt.Errorf("process %s %s/%s: %w", seed.CubeName, slice.Scenario, slice.Time, err)
		}
		totalCells += res.CellsWritten
		totalIssues += len(res.Issues)
		for _, ent := range leaves {
			k := workflow.Key{Cube: seed.CubeName, Entity: ent, Scenario: slice.Scenario, Time: slice.Time}
			for _, action := range []string{workflow.ActionImport, workflow.ActionValidate, workflow.ActionProcess} {
				if _, err := st.Workflow.Apply(k, action, "seed", now); err != nil {
					return fmt.Errorf("workflow %s on %s: %w", action, k.Key(), err)
				}
			}
		}
		processed++
	}

	if err := store.Save(path, st); err != nil {
		return fmt.Errorf("save snapshot: %w", err)
	}
	fmt.Printf("Seeded the %s demo to %s\n", seed.CubeName, path)
	fmt.Printf("  data units: %d, import profiles: %d, workflow entries: %d\n",
		len(st.Cells.Units), len(st.Profiles), len(st.Workflow.Entries))
	fmt.Printf("  consolidated %d slices (both scenarios, 2024–2026): %d cells written, %d issues\n",
		processed, totalCells, totalIssues)
	return nil
}

// runServe loads (or freshly creates) the snapshot, serves the API and UI on
// addr and shuts down gracefully on SIGINT/SIGTERM.
func runServe(path, addr string) error {
	st, err := store.Load(path)
	if err != nil {
		return fmt.Errorf("load snapshot: %w", err)
	}
	srv := server.New(st, path)
	// Optional single-user HTTP Basic Auth via the environment. Both unset
	// leaves the server open (suitable behind a trusted network or for local
	// use); set both to require credentials on every route.
	authUser, authPass := os.Getenv("ONETRICKLE_AUTH_USER"), os.Getenv("ONETRICKLE_AUTH_PASS")
	srv.SetAuth(authUser, authPass)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()
	authState := "off"
	if authUser != "" || authPass != "" {
		authState = "on (user " + authUser + ")"
	}
	log.Printf("onetrickle listening on %s (data: %s, basic-auth: %s)", addr, path, authState)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	case <-ctx.Done():
		stop()
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
