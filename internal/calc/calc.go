// Package calc implements the onetrickle formula DSL (SPEC §6): arithmetic
// over A# account references with ABS/MIN/MAX/IF functions, CPM-style
// division-by-zero → 0 semantics, and topological ordering of formula
// dependencies for the calculation engine.
package calc

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// Resolver returns the value of an A# account reference during Eval.
type Resolver func(account string) (float64, error)

type nodeKind uint8

const (
	nNum  nodeKind = iota // literal; val
	nRef                  // A# reference; name
	nNeg                  // unary minus; args[0]
	nAdd                  // args[0] + args[1]
	nSub                  // args[0] - args[1]
	nMul                  // args[0] * args[1]
	nDiv                  // args[0] / args[1] (0 when divisor is exactly 0)
	nCmp                  // comparison; name = operator, args = [lhs, rhs]; only as IF's first argument
	nCall                 // function call; name = ABS|MIN|MAX|IF, args = arguments
)

type node struct {
	kind nodeKind
	val  float64
	name string
	args []*node
}

// Expr is a parsed formula (opaque AST). Build one with Parse.
type Expr struct {
	root *node
	refs []string // unique A# names, order of first appearance
}

// collectRefs gathers unique A# account names in order of first appearance
// (pre-order, left to right — i.e. source order).
func collectRefs(root *node) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(*node)
	walk = func(nd *node) {
		if nd == nil {
			return
		}
		if nd.kind == nRef && !seen[nd.name] {
			seen[nd.name] = true
			out = append(out, nd.name)
		}
		for _, a := range nd.args {
			walk(a)
		}
	}
	walk(root)
	return out
}

// Refs returns the unique A# account names referenced by the expression, in
// stable order of first appearance. The returned slice is a copy.
func (e *Expr) Refs() []string {
	if e == nil {
		return nil
	}
	out := make([]string, len(e.refs))
	copy(out, e.refs)
	return out
}

// Eval computes the expression's value, resolving each A# reference through
// resolve. Resolver errors abort evaluation and are wrapped with the account
// name. Division by zero yields 0 (CPM convention). Only the taken IF branch
// is evaluated.
func (e *Expr) Eval(resolve Resolver) (float64, error) {
	if e == nil || e.root == nil {
		return 0, errors.New("calc: eval of nil expression")
	}
	return evalNode(e.root, resolve)
}

func evalNode(nd *node, resolve Resolver) (float64, error) {
	switch nd.kind {
	case nNum:
		return nd.val, nil
	case nRef:
		if resolve == nil {
			return 0, fmt.Errorf("calc: no resolver provided for reference A#%s", nd.name)
		}
		v, err := resolve(nd.name)
		if err != nil {
			return 0, fmt.Errorf("calc: resolve A#%s: %w", nd.name, err)
		}
		return v, nil
	case nNeg:
		v, err := evalNode(nd.args[0], resolve)
		if err != nil {
			return 0, err
		}
		return -v, nil
	case nAdd, nSub, nMul, nDiv:
		a, err := evalNode(nd.args[0], resolve)
		if err != nil {
			return 0, err
		}
		b, err := evalNode(nd.args[1], resolve)
		if err != nil {
			return 0, err
		}
		switch nd.kind {
		case nAdd:
			return a + b, nil
		case nSub:
			return a - b, nil
		case nMul:
			return a * b, nil
		default: // nDiv
			if b == 0 {
				return 0, nil
			}
			return a / b, nil
		}
	case nCall:
		return evalCall(nd, resolve)
	default:
		return 0, fmt.Errorf("calc: internal error: unexpected node kind %d", nd.kind)
	}
}

func evalCall(nd *node, resolve Resolver) (float64, error) {
	switch nd.name {
	case "ABS":
		v, err := evalNode(nd.args[0], resolve)
		if err != nil {
			return 0, err
		}
		return math.Abs(v), nil
	case "MIN", "MAX":
		best, err := evalNode(nd.args[0], resolve)
		if err != nil {
			return 0, err
		}
		for _, a := range nd.args[1:] {
			v, err := evalNode(a, resolve)
			if err != nil {
				return 0, err
			}
			if (nd.name == "MIN" && v < best) || (nd.name == "MAX" && v > best) {
				best = v
			}
		}
		return best, nil
	case "IF":
		cond := nd.args[0] // always nCmp by construction
		l, err := evalNode(cond.args[0], resolve)
		if err != nil {
			return 0, err
		}
		r, err := evalNode(cond.args[1], resolve)
		if err != nil {
			return 0, err
		}
		ok, err := compare(cond.name, l, r)
		if err != nil {
			return 0, err
		}
		if ok {
			return evalNode(nd.args[1], resolve)
		}
		return evalNode(nd.args[2], resolve)
	default:
		return 0, fmt.Errorf("calc: internal error: unexpected function %q", nd.name)
	}
}

// compare applies a comparison operator. == and != use exact float equality.
func compare(op string, l, r float64) (bool, error) {
	switch op {
	case "<":
		return l < r, nil
	case "<=":
		return l <= r, nil
	case ">":
		return l > r, nil
	case ">=":
		return l >= r, nil
	case "==":
		return l == r, nil
	case "!=":
		return l != r, nil
	default:
		return false, fmt.Errorf("calc: internal error: unexpected comparison operator %q", op)
	}
}

// TopoSort orders formula accounts so that dependencies evaluate first.
// Map keys are account names; edges run from each formula account to the
// Refs() entries that are also keys (references to non-formula accounts are
// data leaves and impose no ordering). Sibling order is lexicographic, so the
// output is deterministic. A dependency cycle yields an error naming the
// accounts on the cycle.
//
// Callers whose refs resolve through a hierarchy (a ref to an ancestor reads
// the descendants' values too) must expand those implicit dependencies
// themselves and use TopoSortDeps instead.
func TopoSort(formulas map[string]*Expr) ([]string, error) {
	deps := make(map[string][]string, len(formulas))
	for k, e := range formulas {
		var ds []string
		if e != nil {
			for _, r := range e.refs {
				if _, ok := formulas[r]; ok {
					ds = append(ds, r)
				}
			}
		}
		deps[k] = ds
	}
	return TopoSortDeps(deps)
}

// TopoSortDeps orders the keys of deps so that every listed dependency comes
// before its dependent. Dependency entries that are not themselves keys are
// ignored; duplicates are tolerated. Sibling order is lexicographic, so the
// output is deterministic. A dependency cycle yields an error naming the
// accounts on the cycle.
func TopoSortDeps(allDeps map[string][]string) ([]string, error) {
	keys := make([]string, 0, len(allDeps))
	for k := range allDeps {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	deps := make(map[string][]string, len(keys))       // node -> dependencies that are keys
	dependents := make(map[string][]string, len(keys)) // node -> nodes depending on it
	indeg := make(map[string]int, len(keys))
	for _, k := range keys {
		seen := map[string]bool{}
		var ds []string
		for _, d := range allDeps[k] {
			if seen[d] {
				continue
			}
			seen[d] = true
			if _, ok := allDeps[d]; ok {
				ds = append(ds, d)
			}
		}
		sort.Strings(ds)
		deps[k] = ds
		indeg[k] = len(ds)
		for _, d := range ds {
			dependents[d] = append(dependents[d], k)
		}
	}

	// Kahn's algorithm, always taking the lexicographically smallest ready node.
	ready := make([]string, 0, len(keys))
	for _, k := range keys {
		if indeg[k] == 0 {
			ready = append(ready, k)
		}
	}
	order := make([]string, 0, len(keys))
	for len(ready) > 0 {
		sort.Strings(ready)
		k := ready[0]
		ready = ready[1:]
		order = append(order, k)
		for _, m := range dependents[k] {
			indeg[m]--
			if indeg[m] == 0 {
				ready = append(ready, m)
			}
		}
	}
	if len(order) == len(keys) {
		return order, nil
	}

	// Cycle: every unordered node still has an unordered dependency. Walk
	// dependency edges (smallest first) from the smallest unordered node
	// until a node repeats, then report that loop.
	remaining := make(map[string]bool, len(keys)-len(order))
	for _, k := range keys {
		remaining[k] = true
	}
	for _, k := range order {
		delete(remaining, k)
	}
	var start string
	for _, k := range keys {
		if remaining[k] {
			start = k
			break
		}
	}
	index := map[string]int{}
	var path []string
	for cur := start; ; {
		if at, seen := index[cur]; seen {
			cycle := path[at:]
			return nil, fmt.Errorf("calc: formula cycle: %s", strings.Join(append(cycle, cycle[0]), " -> "))
		}
		index[cur] = len(path)
		path = append(path, cur)
		next := ""
		for _, d := range deps[cur] {
			if remaining[d] {
				next = d
				break
			}
		}
		if next == "" { // unreachable; defensive
			return nil, fmt.Errorf("calc: formula cycle involving %s", strings.Join(path, ", "))
		}
		cur = next
	}
}
