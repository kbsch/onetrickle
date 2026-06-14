package calc

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

// almostEq compares floats with an epsilon of 1e-9, applied relatively for
// large magnitudes so exact-arithmetic results at any scale compare cleanly.
func almostEq(a, b float64) bool {
	scale := math.Max(1, math.Max(math.Abs(a), math.Abs(b)))
	return math.Abs(a-b) <= 1e-9*scale
}

// mapResolver resolves accounts from a fixture map; unknown accounts error.
func mapResolver(vals map[string]float64) Resolver {
	return func(account string) (float64, error) {
		v, ok := vals[account]
		if !ok {
			return 0, fmt.Errorf("no data for account %q", account)
		}
		return v, nil
	}
}

func mustParse(t *testing.T, src string) *Expr {
	t.Helper()
	e, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q) failed: %v", src, err)
	}
	return e
}

func TestParseEval(t *testing.T) {
	tests := []struct {
		name string
		src  string
		vals map[string]float64
		want float64
	}{
		// precedence & grouping
		{"mul binds tighter than add", "1+2*3", nil, 7},
		{"parens override precedence", "(1+2)*3", nil, 9},
		{"mixed precedence", "2+3*4-6/3", nil, 12},
		{"left assoc subtraction", "10-3-2", nil, 5},
		{"left assoc division", "100/5/2", nil, 10},
		{"nested parens", "((2))*(3+(4))", nil, 14},

		// unary minus
		{"unary minus", "-5+8", nil, 3},
		{"double unary minus", "--5", nil, 5},
		{"unary minus chain parens", "-(-(-2))", nil, -2},
		{"unary minus inside term", "2*-3", nil, -6},
		{"unary minus on ref", "-A#Sales", map[string]float64{"Sales": 4}, -4},

		// division by zero → 0
		{"div by zero literal", "1/0", nil, 0},
		{"div by zero expression", "5/(2-2)", nil, 0},
		{"div by negative zero", "-3/(1-1)", nil, 0},
		{"div by tiny nonzero divides normally", "7/0.0000000000001", nil, 7e13}, // 1e-13: small but nonzero ⇒ real division, only an exactly-zero divisor yields 0
		{"div by small value", "1/(2e-12)", nil, 5e11},
		{"div by zero ref", "A#X/A#Y", map[string]float64{"X": 9, "Y": 0}, 0},

		// number literals
		{"float literal", "1.5*2", nil, 3},
		{"leading dot float", ".5+1", nil, 1.5},
		{"trailing dot float", "2.*3", nil, 6},
		{"exponent literal", "1.5e2+1", nil, 151},
		{"negative exponent literal", "25e-1", nil, 2.5},
		{"whitespace tolerated", " \t1 +\n2 ", nil, 3},

		// refs
		{"ref simple", "A#Sales*2", map[string]float64{"Sales": 100}, 200},
		{"ref with space", "A#Gross Profit / A#Sales", map[string]float64{"Gross Profit": 40, "Sales": 80}, 0.5},
		{"ref with dots", "A#Acct.1.x+1", map[string]float64{"Acct.1.x": 9}, 10},
		{"ref with hyphen", "A#Co-Op*2", map[string]float64{"Co-Op": 3}, 6},
		{"ref with underscore", "A#Net_Sales - 1", map[string]float64{"Net_Sales": 8}, 7},
		{"ref attached hyphen digit stays in name", "A#X-2 + 1", map[string]float64{"X-2": 5}, 6},
		{"ref at end of string", "1+A#Sales", map[string]float64{"Sales": 41}, 42},
		{"ref with trailing spaces", "A#Sales  ", map[string]float64{"Sales": 7}, 7},
		{"ref spaces and digits", "A#Region 2 * 2", map[string]float64{"Region 2": 11}, 22},
		{"gold gross profit formula", "A#Sales - A#COGS", map[string]float64{"Sales": 1000, "COGS": 600}, 400},
		{"subtraction without spaces", "A#Sales-A#COGS", map[string]float64{"Sales": 10, "COGS": 4}, 6},

		// functions
		{"abs negative", "ABS(3-10)", nil, 7},
		{"abs positive", "ABS(4)", nil, 4},
		{"min multi-arg", "MIN(3, 1, 2)", nil, 1},
		{"max multi-arg", "MAX(3, 1, 2)", nil, 3},
		{"min single arg", "MIN(5)", nil, 5},
		{"max four args with refs", "MAX(A#A, A#B, 0, -1)", map[string]float64{"A": -2, "B": -5}, 0},
		{"min nested exprs", "MIN(2*3, 10-5, 100)", nil, 5},

		// IF with each comparison op
		{"if lt true", "IF(1 < 2, 10, 20)", nil, 10},
		{"if lt false", "IF(2 < 1, 10, 20)", nil, 20},
		{"if le equal", "IF(2 <= 2, 1, 0)", nil, 1},
		{"if gt true", "IF(3 > 2, 1, 0)", nil, 1},
		{"if ge false", "IF(1 >= 2, 1, 0)", nil, 0},
		{"if eq true", "IF(2+2 == 4, 1, 0)", nil, 1},
		{"if eq exact float semantics", "IF(0.1+0.2 == 0.3, 1, 0)", nil, 0},
		{"if ne true", "IF(A#X != 0, 1, 0)", map[string]float64{"X": 0.5}, 1},
		{"if ne false", "IF(0 != 0, 1, 0)", nil, 0},
		{"gpmargin formula", "IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)",
			map[string]float64{"Sales": 200, "GrossProfit": 50}, 25},
		{"gpmargin zero guard", "IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)",
			map[string]float64{"Sales": 0, "GrossProfit": 50}, 0},
		{"nested if outer true inner false", "IF(A#X > 0, IF(A#Y < 5, 1, 2), 3)",
			map[string]float64{"X": 1, "Y": 9}, 2},
		{"nested if outer false", "IF(A#X > 0, IF(A#Y < 5, 1, 2), 3)",
			map[string]float64{"X": -1, "Y": 1}, 3},
		{"if in arithmetic", "10 + IF(1 < 2, 5, 7) * 2", nil, 20},
		{"untaken if branch not evaluated", "IF(1 > 0, 2, A#Missing)", map[string]float64{}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := mustParse(t, tt.src)
			got, err := e.Eval(mapResolver(tt.vals))
			if err != nil {
				t.Fatalf("Eval(%q) failed: %v", tt.src, err)
			}
			if !almostEq(got, tt.want) {
				t.Errorf("Eval(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantSub string // "" = any error
	}{
		{"empty input", "", "empty expression"},
		{"only whitespace", "   ", "empty expression"},
		{"dangling plus", "1+", ""},
		{"dangling star", "2*", ""},
		{"leading star", "*3", ""},
		{"double operator", "1+/2", ""},
		{"bad character", "1 $ 2", "unexpected character"},
		{"lone hash", "#Sales", "unexpected character"},
		{"single equals", "IF(1 = 1, 1, 0)", "unexpected character"},
		{"lone bang", "1 ! 2", "unexpected character"},
		{"underscore number", "1_000", ""},
		{"comparison outside if", "1 < 2", "IF"},
		{"comparison in parens", "(1 < 2)", "IF"},
		{"comparison in min arg", "MIN(1 < 2, 3)", "IF"},
		{"chained comparison in cond", "IF(1 < 2 < 3, 1, 0)", "IF"},
		{"comparison in if value arg", "IF(1 < 2, 3 > 1, 0)", "IF"},
		{"unclosed paren", "(1+2", ""},
		{"unclosed call", "MIN(1, 2", ""},
		{"extra close paren", "1+2)", ""},
		{"empty ref at end", "A#", "empty A# reference"},
		{"empty ref before operator", "A# + 1", "empty A# reference"},
		{"empty ref hyphen start", "A#-Foo", "empty A# reference"},
		{"unknown function", "FOO(1)", "unknown function"},
		{"lowercase function", "abs(1)", "unknown function"},
		{"bare identifier", "Sales + 1", ""},
		{"if cond not comparison", "IF(1, 2, 3)", "comparison"},
		{"if missing else arg", "IF(1 > 0, 2)", ""},
		{"if extra arg", "IF(1 > 0, 1, 2, 3)", ""},
		{"abs two args", "ABS(1, 2)", "ABS"},
		{"abs empty", "ABS()", ""},
		{"trailing junk", "1 2", ""},
		{"top level comma", "1, 2", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, err := Parse(tt.src)
			if err == nil {
				t.Fatalf("Parse(%q) = %v, want error", tt.src, e)
			}
			if tt.wantSub != "" && !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("Parse(%q) error = %q, want substring %q", tt.src, err, tt.wantSub)
			}
		})
	}
}

func TestRefs(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"dedupe and first-appearance order",
			"A#B + A#A * A#B - A#C + IF(A#A > A#D, A#E, 1)",
			[]string{"B", "A", "C", "D", "E"}},
		{"no refs", "1+2*3", []string{}},
		{"single ref repeated", "A#Sales / A#Sales", []string{"Sales"}},
		{"refs in both if branches", "IF(A#C > 0, A#T, A#F)", []string{"C", "T", "F"}},
		{"gold formula", "A#Sales - A#COGS", []string{"Sales", "COGS"}},
		{"spaced names", "A#Gross Profit / A#Sales", []string{"Gross Profit", "Sales"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := mustParse(t, tt.src)
			got := e.Refs()
			if !equalStrings(got, tt.want) {
				t.Errorf("Refs(%q) = %v, want %v", tt.src, got, tt.want)
			}
		})
	}

	t.Run("returned slice is a copy", func(t *testing.T) {
		e := mustParse(t, "A#X + A#Y")
		r := e.Refs()
		r[0] = "mutated"
		if got := e.Refs(); !equalStrings(got, []string{"X", "Y"}) {
			t.Errorf("Refs() after caller mutation = %v, want [X Y]", got)
		}
	})
}

func TestEvalResolverError(t *testing.T) {
	sentinel := errors.New("boom")
	e := mustParse(t, "1 + A#Sales * 2")
	_, err := e.Eval(func(account string) (float64, error) {
		return 0, sentinel
	})
	if err == nil {
		t.Fatal("Eval with failing resolver: want error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("Eval error = %v, want wrapped sentinel (errors.Is)", err)
	}
	if !strings.Contains(err.Error(), "A#Sales") {
		t.Errorf("Eval error = %q, want mention of A#Sales", err)
	}
}

func TestEvalNilResolver(t *testing.T) {
	t.Run("refs require resolver", func(t *testing.T) {
		e := mustParse(t, "A#X")
		if _, err := e.Eval(nil); err == nil {
			t.Error("Eval(nil) on expression with refs: want error, got nil")
		}
	})
	t.Run("pure expression works without resolver", func(t *testing.T) {
		e := mustParse(t, "2*3+1")
		got, err := e.Eval(nil)
		if err != nil {
			t.Fatalf("Eval(nil) failed: %v", err)
		}
		if !almostEq(got, 7) {
			t.Errorf("Eval(nil) = %v, want 7", got)
		}
	})
}

func TestTopoSort(t *testing.T) {
	tests := []struct {
		name     string
		formulas map[string]string
		want     []string
		wantErr  []string // non-nil = expect error containing each part
	}{
		{"empty map", map[string]string{}, []string{}, nil},
		{"single formula no deps", map[string]string{"A": "1+2"}, []string{"A"}, nil},
		{"two chain gold", map[string]string{
			"NetIncome":   "A#GrossProfit - A#OpEx",
			"GrossProfit": "A#Sales - A#COGS",
		}, []string{"GrossProfit", "NetIncome"}, nil},
		{"three chain", map[string]string{
			"C": "A#B+1",
			"B": "A#A+1",
			"A": "A#Data",
		}, []string{"A", "B", "C"}, nil},
		{"diamond", map[string]string{
			"Top":   "A#Left + A#Right",
			"Left":  "A#Base * 2",
			"Right": "A#Base * 3",
			"Base":  "A#Data",
		}, []string{"Base", "Left", "Right", "Top"}, nil},
		{"independents sorted lexicographically", map[string]string{
			"Zeta":  "1",
			"Mid":   "2",
			"Alpha": "3",
		}, []string{"Alpha", "Mid", "Zeta"}, nil},
		{"data refs ignored for ordering", map[string]string{
			"X": "A#Alpha + A#Beta",
			"Y": "A#Gamma / 2",
		}, []string{"X", "Y"}, nil},
		{"mixed data and formula refs", map[string]string{
			"Margin": "A#Profit / A#Sales",
			"Profit": "A#Sales - A#COGS",
		}, []string{"Profit", "Margin"}, nil},
		{"two node cycle", map[string]string{
			"A": "A#B",
			"B": "A#A",
		}, nil, []string{"cycle", "A -> B -> A"}},
		{"self cycle", map[string]string{
			"S": "A#S + 1",
		}, nil, []string{"cycle", "S -> S"}},
		{"cycle with tail excluded", map[string]string{
			"Tail": "A#A",
			"A":    "A#B",
			"B":    "A#C",
			"C":    "A#A",
		}, nil, []string{"cycle", "A -> B -> C -> A"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			formulas := make(map[string]*Expr, len(tt.formulas))
			for k, src := range tt.formulas {
				formulas[k] = mustParse(t, src)
			}
			got, err := TopoSort(formulas)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("TopoSort = %v, want error", got)
				}
				for _, part := range tt.wantErr {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("TopoSort error = %q, want substring %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("TopoSort failed: %v", err)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("TopoSort = %v, want %v", got, tt.want)
			}
			// Dependencies must precede their dependents.
			pos := map[string]int{}
			for i, k := range got {
				pos[k] = i
			}
			for k := range tt.formulas {
				for _, r := range formulas[k].Refs() {
					if _, isFormula := formulas[r]; isFormula && pos[r] > pos[k] {
						t.Errorf("dependency %q ordered after dependent %q in %v", r, k, got)
					}
				}
			}
		})
	}
}

func TestTopoSortDeps(t *testing.T) {
	tests := []struct {
		name    string
		deps    map[string][]string
		want    []string
		wantErr []string
	}{
		{"empty", map[string][]string{}, []string{}, nil},
		{"chain", map[string][]string{
			"C": {"B"}, "B": {"A"}, "A": nil,
		}, []string{"A", "B", "C"}, nil},
		{"non-key deps ignored", map[string][]string{
			"X": {"Data1", "Data2"}, "Y": {"X", "Data3"},
		}, []string{"X", "Y"}, nil},
		{"duplicate deps tolerated", map[string][]string{
			"B": {"A", "A", "A"}, "A": nil,
		}, []string{"A", "B"}, nil},
		{"expanded ancestor dependency orders formula first", map[string][]string{
			// AAA refs a parent whose subtree contains GP: explicit edge AAA->GP.
			"AAA": {"GP"}, "GP": nil,
		}, []string{"GP", "AAA"}, nil},
		{"self dependency is a cycle", map[string][]string{
			"S": {"S"},
		}, nil, []string{"cycle", "S -> S"}},
		{"two node cycle", map[string][]string{
			"A": {"B"}, "B": {"A"},
		}, nil, []string{"cycle", "A -> B -> A"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := TopoSortDeps(tt.deps)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("TopoSortDeps = %v, want error", got)
				}
				for _, part := range tt.wantErr {
					if !strings.Contains(err.Error(), part) {
						t.Errorf("TopoSortDeps error = %q, want substring %q", err, part)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("TopoSortDeps failed: %v", err)
			}
			if !equalStrings(got, tt.want) {
				t.Errorf("TopoSortDeps = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTopoSortDeterministic(t *testing.T) {
	formulas := map[string]*Expr{
		"Top":   mustParse(t, "A#Left + A#Right"),
		"Left":  mustParse(t, "A#Base * 2"),
		"Right": mustParse(t, "A#Base * 3"),
		"Base":  mustParse(t, "A#Data"),
	}
	want := []string{"Base", "Left", "Right", "Top"}
	for i := 0; i < 50; i++ {
		got, err := TopoSort(formulas)
		if err != nil {
			t.Fatalf("TopoSort failed: %v", err)
		}
		if !equalStrings(got, want) {
			t.Fatalf("run %d: TopoSort = %v, want %v", i, got, want)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
