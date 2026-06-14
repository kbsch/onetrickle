# onetrickle — Specification

A Go-based clone of OneStream (the CPM platform): dimensional cubes, financial
consolidation with FX translation and intercompany eliminations, a calculation
engine, staged data import with transformation rules, workflow tracking, and a
web UI. Single binary, **stdlib only** (no external dependencies), JSON-file
persistence.

This document is the contract between packages. When code and SPEC disagree,
fix one of them — never let them drift silently.

## 1. Glossary

- **POV** (point of view): a full coordinate naming one cell.
- **Data unit**: all cells for one (cube, entity, scenario, month). Loading and
  consolidation operate per data unit.
- **Stage** (consolidation stage): Local → Translated → Elimination →
  Consolidated. `Local` is what users enter (entity local currency); the other
  stages are materialized by the consolidation engine in group currency.
- **Group currency**: the cube's reporting currency (e.g. USD). Translation is
  single-level: everything is translated local → group (a documented
  simplification vs. OneStream's level-by-level parent-currency translation).

## 2. Dimensions

| Dim       | Managed by   | Notes |
|-----------|--------------|-------|
| Entity    | user         | tree; each member has `Currency`, `OwnershipPct` (consolidation % into its parent, default 100) |
| Account   | user         | tree; `AccountType`, `IsIC`, `Formula`, `DynamicCalc`, edge `Weight` (+1/-1) |
| Scenario  | user         | flat (Actual, Budget, Forecast, …) |
| Time      | generated    | `2025` → `2025Q1..Q4` → `2025M1..M12`. Only months store data. |
| View      | fixed        | `Periodic`, `YTD` (derived at query time, never stored) |
| Flow      | user         | tree, must contain root `None` (default) |
| Origin    | fixed        | flat: `Import`, `Forms`, `Adj` (user-writable), `Calc` (engine: stored-calc results), `Elim` (engine: elimination entries, only in stage maps) |
| IC        | implicit     | `None` or the name of an Entity member. Not a stored Dimension; validated against the Entity dim. |
| UD1..UD4  | user         | trees, each must contain root `None` (default) |

Member names: must match `^[A-Za-z0-9][A-Za-z0-9 ._\-]*$` (in particular no
`|` or `#`) and must additionally be referenceable as a calc `A#` reference
(§6): no trailing space, and every hyphen tightly bound between name-word
characters (letters, digits, `.` or `_`) — i.e. no leading, trailing or
space-adjacent hyphen. Names are unique within their dimension. Single-parent
hierarchies only (no shared/alternate hierarchies in MVP).

### Account types

| AccountType  | Time agg | YTD view        | FX rate  |
|--------------|----------|-----------------|----------|
| Revenue      | Sum      | cumulative sum  | Average  |
| Expense      | Sum      | cumulative sum  | Average  |
| Flow         | Sum      | cumulative sum  | Average  |
| NonFinancial | Sum      | cumulative sum  | Average  |
| Asset        | Last     | value at month  | Closing  |
| Liability    | Last     | value at month  | Closing  |
| Equity       | Last     | value at month  | Closing  |

"Time agg = Last" means: querying `2025Q1` or `2025` returns the value of the
**final month of that period** (M3 / M12), even if zero. No natural-sign
flipping: values are stored as entered; report polarity is modeled with edge
weights (e.g. Expense children of NetIncome carry Weight -1).

### Hierarchy aggregation (non-entity dims)

Aggregated value of member M = Σ over children c of (value(c) × c.Weight),
recursively; leaves read storage. Entity is special: parent-entity values come
from that entity's own materialized stage maps (consolidation IS the entity
rollup), never from summing children at query time.

## 3. Storage model (`internal/cube`)

```go
type UnitKey struct{ Cube, Entity, Scenario, Time string } // Time is a month member
type CellCoord struct{ Account, Flow, Origin, IC, UD1, UD2, UD3, UD4 string }
```

String codecs (used in JSON persistence): `UnitKey.Key()` →
`"cube|entity|scenario|time"`, `CellCoord.Key()` →
`"acct|flow|origin|ic|ud1|ud2|ud3|ud4"`. Parse functions reverse them.

```go
type Unit struct {
    Input  map[CellCoord]float64                       // Local stage: Origin ∈ {Import, Forms, Adj, Calc}
    Stages map[model.ConsStage]map[CellCoord]float64   // Translated / Elimination / Consolidated
}
type Store struct { /* RWMutex + Units map[UnitKey]*Unit */ }
```

Write rules: only **leaf** members of each dim (months for Time), Origin must
be `Import`/`Forms`/`Adj` for user writes. Accounts with a formula (stored or
dynamic) are engine-owned and reject user writes and imports. Values must be
finite (NaN/±Inf is rejected at every trust boundary — a non-finite cell
would poison JSON persistence). Setting value 0 deletes the cell. Import uses
**replace semantics**: loading a unit first clears that unit's
`Origin=Import` cells.

## 4. Query semantics (`internal/cube` Engine)

```go
type POV struct {
    Cube, Entity, Scenario, Time, View, Stage string
    Account, Flow, Origin, IC, UD1, UD2, UD3, UD4 string
}
```

Defaults when empty: View=Periodic, Stage=Consolidated, Flow/UD*=total of the
dim (sum over roots), Origin="" = sum over all origins present, IC="" = sum
over all partners (IC="None" matches only the None partner).

`Engine.GetCell(meta, pov) float64` resolution order:

1. **Dynamic-calc account** → parse+eval its formula; `A#X` refs resolve via
   GetCell with the account swapped. Recursion guard: re-entering an (account,
   POV) already being evaluated is a cycle — the cycling cell yields 0; an
   acyclic chain deeper than 16 yields 0 at the cut-off. Cycles, depth-limit
   hits and evaluation errors are recorded as issues (surfaced via
   `GetCellIssues` and `QueryResult.Issues`). Evaluation happens AFTER
   aggregation (post-rollup).
2. **Time**: month → direct; quarter/year → aggregate the months under it per
   the account's time agg (Sum / Last). View=YTD: months Jan..M of that year
   (Sum accounts cumulative; Last accounts = value at M).
3. **Entity**: leaf or parent both read that entity's own unit. Stage=Local
   reads Input; other stages read `Stages[stage]`. Stage=Consolidated on a
   leaf entity with no materialized stages falls back to Translated, then —
   only when the entity's currency equals the cube group currency — to Input.
4. **Account/Flow/UD**: weighted hierarchy rollup over the member and all of
   its descendants (stored cells normally live only at leaves per §3; a cell
   stored at the queried member itself counts with weight 1). Stored-calc
   results live at Origin=Calc and are summed in like any origin.

`Engine.Query(meta, QueryRequest) (*QueryResult, error)` renders a grid:

```go
type AxisSpec struct { Dim string; Member string; Expand string } // Expand: member|children|leaves|tree
type HeaderPart struct { Dim, Name string; Depth int; IsLeaf bool } // one nesting level of a tuple
type QueryRequest struct {
    Cube string; POV POV
    Rows, Cols       []AxisSpec   // single flat level (specs concatenate)
    RowNest, ColNest [][]AxisSpec // nesting levels (outer→inner); override Rows/Cols
}
type QueryResult struct {
    RowHeaders []HeaderCell   // {Name, Depth, IsLeaf}; innermost level of each position
    ColHeaders []HeaderCell
    Cells      [][]float64    // [row][col]
    Issues     []string       // dynamic-calc problems (deduplicated, never nil)
    RowPaths   [][]HeaderPart // per-position tuples; set only for a nested axis (>1 level)
    ColPaths   [][]HeaderPart
}
```

An axis is a list of **nesting levels** (outer→inner); the rendered axis is the
**cross product** of the levels, and each level is a set of stacked AxisSpecs
that **concatenate**. `Rows`/`Cols` give a single (flat) level — the legacy
shape; `RowNest`/`ColNest`, when non-empty, give the full nested form and take
precedence. A nested cell overlays every level's member (outer first) onto the
POV, so e.g. rows `Entity × Account` yields one row per (entity, account) pair.
`tree` = member followed by all descendants, Depth = distance from the spec
member (for indentation). `RowPaths`/`ColPaths` carry the per-position tuple and
are populated only when an axis has more than one level; the flat
`RowHeaders`/`ColHeaders` always hold the innermost level. Backward compatible:
a single-level axis behaves exactly as before and emits no paths.

## 5. Consolidation (`internal/consol`)

`Process(meta, store, cube, scenario, month) (*Result, error)` — full-cube
materialization for one (scenario, month). Steps, in order:

1. **Stored calcs** (per entity unit): clear Origin=Calc cells; evaluate
   non-dynamic account formulas in topological order of their `A#` references.
   Stored formulas must target **leaf** accounts (a Calc cell at a non-leaf
   would double-count with its children in every rollup) — a non-leaf target
   is an error, abort. Because a ref resolves as the rollup of the referenced
   account's whole subtree, a ref to an ancestor of another formula account
   is an implicit dependency on that formula; the dependency graph is
   expanded accordingly before ordering (cycle — direct or via ancestors →
   error, abort). For each formula account: the set of "rest-tuples"
   (CellCoord minus Account, Origin collapsed) is the union of tuples where
   any referenced account has data; for each tuple, eval with refs = local
   weighted-rollup value of the referenced account at that tuple (sum across
   origins); write result to (Account=target, Origin=Calc, rest=tuple),
   skipping zeros; a non-finite result records an Issue and is skipped.
2. **Translate** (per entity): clear+rebuild `Stages[Translated]` = every
   Input cell × rate(entity.Currency → group, by account's RateType, for this
   scenario/month). Same-currency rate is 1. Missing rate → record an Issue,
   fall back to 1.0.
3. **Eliminations**: for every translated cell with IC ≠ None whose account
   has IsIC: find FCA = first common ancestor of (entity, IC partner) in the
   entity tree; if none, record Issue and skip; else accumulate
   `pending[FCA] += (coord with Origin=Elim, value = -translatedValue)`.
   After all entities: `Stages[Elimination]` of each FCA = its pendings.
   Eliminations are posted at **full value** regardless of ownership %
   (documented simplification).
4. **Consolidate** (post-order over entity tree):
   `Consolidated[E] = Translated[E] + Elimination[E] + Σ_children (Consolidated[c] × c.OwnershipPct/100)`.
   Skip zero cells.

`Result` carries `Issues []string` and counts (units touched, cells written).
At every step, a computed value that is not finite (NaN/±Inf from arithmetic
overflow) is skipped with an Issue recorded — non-finite cells must never
reach the store. Re-running Process is idempotent (stages and Calc origin are
rebuilt).

### Gold fixtures (use in tests — exact expected values)

Entities: `Global(USD)` → `US(USD, 100%)`, `DE(EUR, 100%)`. Accounts:
`Sales(Revenue, IsIC)`, `COGS(Expense, IsIC)`, `Cash(Asset)`. Rates
Actual/2025M1: EUR Average=1.10, Closing=1.08. Input (Origin=Import, other
dims None): US: Sales=1000, Sales[IC=DE]=200, Cash=500. DE (EUR): Sales=400,
COGS[IC=US]=180, Cash=300.

Expected (Stage=Consolidated, 2025M1, Periodic):
- US: Sales=1200, Cash=500
- DE: Sales=440 (400×1.10), COGS=198 (180×1.10), Cash=324 (300×1.08)
- Global: Sales = 1200 + 440 − 200 = **1440**; COGS = 198 − 198 = **0**;
  Cash = 500 + 324 = **824**
- Global Stage=Elimination: Sales[IC=DE] = −200, COGS[IC=US] = −198

Fixture B (ownership): same but DE OwnershipPct=80 and **no IC cells**
(US: Sales=1000, Cash=500; DE: Sales=400, Cash=300). Global: Sales = 1000 +
0.8×440 = **1352**; Cash = 500 + 0.8×324 = **759.2**.

## 6. Calc DSL (`internal/calc`)

Grammar (float64 semantics):

```
expr   := term (('+'|'-') term)*
term   := factor (('*'|'/') factor)*
factor := NUMBER | REF | FUNC '(' expr (',' expr)* ')' | '(' expr ')' | '-' factor
REF    := 'A#' MemberName        (MemberName may contain interior spaces, dots and tightly-bound hyphens; ends before an operator/paren/comma)
FUNC   := ABS | MIN | MAX | IF
cond (only inside IF's first arg) := expr ('<'|'<='|'>'|'>='|'=='|'!=') expr
```

`IF(cond, a, b)`; division by zero → 0 (not NaN/Inf — CPM convention).
Public API:

```go
type Resolver func(account string) (float64, error)
func Parse(src string) (*Expr, error)
func (e *Expr) Refs() []string                      // unique A# account names
func (e *Expr) Eval(resolve Resolver) (float64, error)
func TopoSort(formulas map[string]*Expr) ([]string, error)  // edges from exact-name refs; cycle → error naming the cycle
func TopoSortDeps(deps map[string][]string) ([]string, error) // explicit edges (callers expand hierarchy-implied deps)
```

## 7. Stage / import (`internal/stage`)

```go
type RuleKind string // "exact", "prefix" ("41*"), "default" ("*")
type Rule struct{ Dim model.DimType; Kind RuleKind; Src, Target string }
type ColumnSpec struct{ Col int; Fixed string }   // Col=-1 → use Fixed
type Profile struct {
    Name, Cube string
    HasHeader  bool
    Delimiter  string                      // default ","
    Columns    map[model.DimType]ColumnSpec // for Entity, Account, Scenario, Time, Flow, IC, UD1..4
    AmountCol  int
    Rules      []Rule
}
func Transform(p *Profile, csvData []byte) (*TransformResult, error)
```

Resolution per dim per raw value: exact match wins; else longest matching
prefix rule; else default rule; else the raw value passes through unchanged
(**identity fallback** — mapping is optional; a raw value naming a valid leaf
member loads without any rule). Amounts must parse as finite numbers: NaN and
±Inf literals are flagged as bad amounts (row retained, flagged).
`TransformResult` has `Rows []MappedRow` (each: dim values, amount, source
line, issues) and `Issues []string`. Rows whose Time isn't a month or whose
members don't exist / aren't leaves are flagged by
`Validate(meta, *TransformResult)`. `LoadPlan(res) map[cube.UnitKey][]CellWrite`
groups clean rows (Origin=Import, summing duplicate coords); the server
additionally rejects a commit whose summed amounts overflow to ±Inf.

## 8. Workflow (`internal/workflow`)

Key = (Cube, Entity, Scenario, Time-month). States:
`NotStarted → Imported → Validated → Processed → Certified`.

Actions: `import` (from any non-certified state → Imported), `validate`
(Imported→Validated), `process` (Validated→Processed), `certify`
(Processed→Certified), `reopen` (any → NotStarted). Invalid transition →
error. Entries record UpdatedAt/UpdatedBy and append-only History
`[]Event{Action, From, To, At, By}`. Certified units reject data writes and
imports (enforced in server).

## 9. Persistence (`internal/store`)

Single JSON snapshot `<datadir>/onetrickle.json`:

```json
{ "version": 1, "metadata": {...}, "units": {"<unitkey>": {"input": {"<coordkey>": 1.0}, "stages": {...}}},
  "profiles": {...}, "workflow": {...} }
```

`Save(path, *AppState) error` (atomic: temp file + rename, 0644),
`Load(path) (*AppState, error)` (missing file → fresh state). `AppState`
bundles `*model.Metadata`, `*cube.Store`, `map[string]*stage.Profile`,
`*workflow.Registry`. Server saves after every successful mutation.

## 10. REST API (`internal/server`)

JSON; errors as `{"error": "..."}` with 4xx/5xx. Optional single-user HTTP
Basic Auth guards every route (UI and API) when `ONETRICKLE_AUTH_USER` and
`ONETRICKLE_AUTH_PASS` are set; unset (the default) leaves the server open.
Credentials compare in constant time; a failed/missing login gets 401 with a
`WWW-Authenticate: Basic` challenge. TLS is expected to terminate at a reverse
proxy (see `deploy/`).

| Method+Path | Body → Response |
|---|---|
| GET `/api/health` | `{"ok":true}` |
| GET `/api/meta` | `{cubes, scenarios, years, currencies, latestDataTime}` summary (`latestDataTime` = latest month with any stored data, `""` if none — UI default-POV hint) |
| GET `/api/dims/{type}/members` | full tree: `[{name,parent,depth,weight,props...}]` (pre-order) |
| POST `/api/dims/{type}/members` | `{name,parent,weight,accountType,isIC,currency,ownershipPct,...}` — Scenario stays flat (no parent); no children under stored-calc accounts |
| PUT `/api/dims/{type}/members/{name}` | same fields, partial update; same flatness/leaf rules; the `None` roots of Flow/UD1..4 cannot be re-parented |
| DELETE `/api/dims/{type}/members/{name}?recursive=1` | refuses the mandatory `None` roots (400) and members still referenced by stored Input cells — as coordinate, unit key or IC partner (409); engine-derived leftovers (stage cells, workflow entries) are purged |
| GET `/api/rates?scenario=S&time=T` | `[{currency,type,value}]` |
| PUT `/api/rates?scenario=S&time=T` | same shape in |
| POST `/api/data/cells` | `[{unit:{cube,entity,scenario,time}, coord:{account,...}, value}]` — rejects certified units, formula accounts (stored or dynamic) and non-finite values |
| POST `/api/query` | QueryRequest → QueryResult (§4, incl. `issues`; `rowNest`/`colNest` for nested pivots → `rowPaths`/`colPaths`) |
| GET `/api/export?cube=&scenario=&time=&stage=` | CSV of non-zero consolidated leaf cells |
| GET/POST `/api/profiles`, PUT/DELETE `/api/profiles/{name}` | import profiles |
| POST `/api/import/preview` | multipart `file` + `profile` → TransformResult (first 200 rows) + issues. Uploads are capped (20 MB body / 200k rows → 413) |
| POST `/api/import/commit` | same → loads clean rows, workflow → Imported, returns counts; same size caps; non-finite summed amounts → 400 |
| GET `/api/workflow?cube=&scenario=&time=` | entries for all entities |
| POST `/api/workflow/action` | `{cube,entity,scenario,time,action}`; the transition is validated FIRST (illegal → 409 with no side effect); a legal `process` runs consolidation (§5) |
| POST `/api/process` | `{cube,scenario,time}` → consol Result (admin/manual trigger) |
| GET `/api/formulas?cube=` | accounts with formulas |
| PUT `/api/formulas/{account}` | `{formula,dynamic}` — parse-validates before saving; stored (non-dynamic) formulas only on leaf accounts |

Static UI served at `/` from embedded `web/` (go:embed). Binary: `onetrickle
-data ./data -addr :8080` plus subcommand `onetrickle seed -data ./data`
(creates the GolfTrickle demo, refusing to overwrite existing data).

## 11. Web UI (`web/` — vanilla JS SPA, no build step)

Hash-routed pages; `fetch` against the API; shared POV selector bar
(cube/scenario/time). Pages:

- **Dashboard** — KPI tiles (configurable accounts persisted in localStorage,
  default Sales/GrossProfit/NetIncome/GPMargin at top entity,
  Consolidated/YTD) + workflow completion summary. The default POV time is
  the server's `latestDataTime` hint so a fresh load shows numbers.
- **Quick View** — the pivot grid: each axis (rows, columns) is a list of
  dimension levels you add/remove/reorder (dim+member+expand per level); the
  grid renders their cross product with nested headers merged via
  rowspan/colspan. POV supplies the rest (a POV member filter is disabled when
  its dimension is on an axis). Renders QueryResult with indentation (query
  `issues` listed above the grid); cells editable only when the full POV is
  leaf-level Local input at a specific user origin (Import/Forms/Adj — replace-
  what-you-see; aggregated/parent cells and formula accounts are read-only).
  Writes via /api/data/cells, then re-query. Export CSV button.
- **Workflow** — entity × status board with action buttons per the state
  machine (illegal actions disabled); "process" shows returned Issues.
- **Metadata** — dimension picker + member tree; add/edit/delete members and
  their properties.
- **Rates** — editable currency × rate-type grid per scenario/period.
- **Import** — profile editor (columns, rules) + file upload → preview table
  with issues highlighted → commit.
- **Formulas** — list/edit account formulas, dynamic flag, server-side parse
  validation feedback.

Look: clean light, near-monochrome theme (OneStream desktop style) — sharp
corners, thin gray borders, white content, gray chrome, blue reserved for
selection/focus. System font stack, no frameworks, no CDN (must work offline).

## 12. Package map & ownership

```
cmd/onetrickle/        main: flags, seed subcommand, wiring     (server agent)
internal/model/        dimensions, members, time, rates, meta   (HAND-AUTHORED — read, don't rewrite)
internal/cube/         types.go HAND-AUTHORED; engine.go query  (cube agent)
internal/calc/         DSL parser/eval/topo                     (calc agent)
internal/consol/       translation, elims, consolidation        (consol agent)
internal/stage/        profiles, transform, validate, load      (stage agent)
internal/workflow/     state machine + registry                 (workflow agent)
internal/store/        snapshot load/save                       (store agent)
internal/seed/         GolfTrickle demo data                    (seed agent)
internal/server/       HTTP API + embed                         (server agent)
web/                   SPA                                      (ui agent)
```

## 13. Coding standards

- Go stdlib ONLY. No third-party modules, no CGO, no CDN assets.
- gofmt-clean; table-driven tests; wrap errors with `%w` and context.
- No package-level mutable state; concurrency: the server holds one
  `sync.RWMutex` over AppState — inner packages may assume single-writer.
- Floats: float64; comparisons in tests use an epsilon of 1e-9.
- Any process you spawn (servers, etc.) you must terminate before finishing.

## 14. GolfTrickle demo (seed)

Cube `GolfTrickle` (USD). Entities: GolfTrickle Inc(USD) → North America(USD)
→ {US Operations(USD), Canada(CAD)}; Europe(EUR) → {Germany(EUR),
France(EUR)}. Accounts: NetIncome = GrossProfit(+1) + OpEx(-1) where
GrossProfit (stored calc `A#Sales - A#COGS`); Sales(Revenue, IsIC),
COGS(Expense, IsIC), OpEx(Expense) → {Salaries, Marketing, Rent};
BalanceSheet → Cash(Asset), Receivables(Asset), Payables(Liability);
GPMargin (DynamicCalc `IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)`,
NonFinancial). Scenarios: Actual, Budget. Time: 2024–2026. Rates for every
month of 2024–2026 (CAD ≈ 0.74 avg / 0.73 close, EUR ≈ 1.09 avg / 1.08 close
at the 2025 baseline, with slight monthly drift and a per-year shift). Data:
both Actual and Budget for every month of 2024, 2025 and 2026 for all leaf
entities, scaled per year (2024 ≈ 0.9×, 2026 ≈ 1.1× the 2025 baseline so the
years differ), including an IC pair each month: US Operations Sales[IC=Germany]
and Germany COGS[IC=US Operations]. Canada ownership 100%, France 80%. The 2025
figures are exactly the single-year baseline, so the gold consolidation numbers
still hold. Seed consolidates every (scenario, month) slice and walks each leaf
entity's workflow to Processed — every period shows consolidated numbers
immediately and no unit is certified.
