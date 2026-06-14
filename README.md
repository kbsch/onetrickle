# onetrickle

# (started as a Fable 5 oneshot test, not a serious repo)

A CPM (Corporate Performance Management) clone in pure Go — single
binary, stdlib only, no CGO, no CDN assets, JSON-file persistence. It does
dimensional cubes, financial consolidation with FX translation and
intercompany eliminations, a calculation DSL, staged CSV import with
transformation rules, workflow tracking, and an embedded web UI.

## Features

- **Dimensional cube engine** — Entity / Account / Scenario / Time / View /
  Flow / Origin / IC / UD1–UD4, weighted hierarchy rollups, Periodic and YTD
  views, Sum/Last time aggregation per account type.
- **Consolidation engine** — stored calcs (topologically ordered), local →
  group-currency FX translation (Average/Closing rates per account type),
  intercompany eliminations posted at the first common ancestor, ownership-%
  weighted post-order consolidation. Idempotent and deterministic.
- **Calc DSL** — `A#Sales - A#COGS`, `IF(A#Sales == 0, 0, A#GrossProfit /
  A#Sales * 100)`, with `ABS`/`MIN`/`MAX`/`IF`, stored or dynamic (query-time)
  formulas, cycle detection.
- **Staged import** — CSV import profiles with column mapping and exact /
  prefix / default transformation rules, validation with per-row issues,
  preview-then-commit with replace semantics.
- **Workflow** — per-(cube, entity, scenario, month) state machine: NotStarted
  → Imported → Validated → Processed → Certified, with append-only history.
  Certified units reject writes and imports.
- **REST API + embedded SPA** — vanilla-JS, hash-routed, works offline;
  Dashboard, Quick View grid (editable cells), Workflow board, Metadata
  editor, FX Rates, Import, Formulas pages.
- **Persistence** — one atomic, byte-deterministic JSON snapshot
  (`<datadir>/onetrickle.json`).

## Quickstart

```sh
go build -o onetrickle ./cmd/onetrickle
./onetrickle seed -data ./data        # creates the GolfTrickle demo (refuses to overwrite)
./onetrickle -data ./data -addr :8080 # serve API + UI
```

Open <http://localhost:8080> in a browser.

## The GolfTrickle demo tour

The seed builds cube **GolfTrickle** (USD group currency): GolfTrickle Inc →
North America → {US Operations (USD), Canada (CAD)} and Europe → {Germany
(EUR), France (EUR, 80% owned)}, with Actual data for 2025M1–M6, Budget for
2025M1–M12, FX rates for all of 2025, an intercompany Sales/COGS pair between
US Operations and Germany, and Actual 2025M1–M3 already consolidated. Three
things to try:

1. **Quick View** — set rows to Account / NetIncome / tree and cols to Time /
   2025Q1 / leaves with entity GolfTrickle Inc, stage Consolidated. You'll see
   group-currency consolidated numbers with the IC Sales/COGS elimination
   already netted out. To see the −200-style elimination entries, switch Stage
   to Elimination and set rows to Account / Sales (or COGS) — eliminations
   post to the IC accounts, which sit outside the NetIncome tree. Then switch
   the entity to Canada, stage to Local and Origin to Forms, and double-click
   a leaf cell to edit it.
2. **Workflow** — pick Actual / 2025M4 (loaded but not yet processed): walk a
   leaf entity through import → validate → process → certify, then try
   writing to the certified unit from Quick View and watch the server refuse
   it (409).
3. **Import** — on the Import page pick the "GolfTrickle CSV" profile and
   upload a CSV like the seed sample (entity, account like `4100`/`5xxx`,
   month, amount — the rules map `4100`→Sales and `5*`→COGS). Preview shows
   mapped rows with issues highlighted; commit loads them and flips the
   workflow to Imported.

## API examples

```sh
curl -s localhost:8080/api/meta

curl -s -X POST localhost:8080/api/query -H 'Content-Type: application/json' -d '{
  "cube":"GolfTrickle",
  "pov":{"scenario":"Actual","time":"2025M1","stage":"Consolidated","entity":"GolfTrickle Inc"},
  "rows":[{"dim":"Account","member":"NetIncome","expand":"tree"}],
  "cols":[{"dim":"Time","member":"2025Q1","expand":"leaves"}]}'

curl -s -X POST localhost:8080/api/process -H 'Content-Type: application/json' \
  -d '{"cube":"GolfTrickle","scenario":"Actual","time":"2025M4"}'
```

Full endpoint table in [SPEC.md](SPEC.md) §10.

## Architecture

```
cmd/onetrickle/     CLI: flags, seed subcommand, graceful shutdown
internal/model/     dimensions, members, accounts, time, FX rates, metadata
internal/cube/      cell storage (units/coords) + the query engine (GetCell/Query)
internal/calc/      formula DSL: lexer, parser, evaluator, topological sort
internal/consol/    stored calcs, translation, eliminations, consolidation
internal/stage/     import profiles, CSV transform, validate, load plan
internal/workflow/  per-unit state machine + registry
internal/store/     atomic JSON snapshot load/save (AppState)
internal/server/    REST API, embedded UI serving, the one RWMutex
internal/seed/      GolfTrickle demo data
web/                vanilla-JS SPA (go:embed, no build step)
```

The server holds a single `sync.RWMutex` over the whole `AppState`; inner
packages assume single-writer. Every successful mutation is snapshotted to
disk before the response is sent.

## Simplifications vs. real OneStream

Deliberate MVP simplifications, all documented in SPEC.md:

- **Single group-currency translation** — everything translates local → group
  in one hop (no level-by-level parent-currency translation).
- **Full-value eliminations** — IC eliminations post at 100% regardless of
  ownership percentage.
- **No CTA** — no cumulative translation adjustment / FX plug accounts.
- **No alternate hierarchies** — single-parent trees only, no shared members.
- **No auth** — self-hosted, trusted-network MVP; there are no users or
  permissions (workflow `by` is a free-text label).

## License

MIT (do whatever).
