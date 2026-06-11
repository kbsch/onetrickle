package calc

import (
	"fmt"
	"strconv"
	"strings"
)

// ---- lexer ----

type tokKind uint8

const (
	tkEOF tokKind = iota
	tkNum
	tkRef
	tkIdent
	tkPlus
	tkMinus
	tkStar
	tkSlash
	tkLParen
	tkRParen
	tkComma
	tkCmp
)

type token struct {
	kind tokKind
	text string  // operator text, identifier, member name (tkRef), literal text (tkNum)
	val  float64 // tkNum only
	pos  int     // byte offset of the token in the source
}

// String renders a token for error messages.
func (t token) String() string {
	switch t.kind {
	case tkEOF:
		return "end of expression"
	case tkNum:
		return fmt.Sprintf("number %q", t.text)
	case tkRef:
		return fmt.Sprintf("reference %q", "A#"+t.text)
	case tkIdent:
		return fmt.Sprintf("identifier %q", t.text)
	default:
		return fmt.Sprintf("%q", t.text)
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
func isAlpha(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }
func isAlnum(c byte) bool { return isAlpha(c) || isDigit(c) }

// isNameWord reports whether c unconditionally continues a member name
// (letters, digits, dot, underscore — every name char except space and hyphen).
func isNameWord(c byte) bool { return isAlnum(c) || c == '.' || c == '_' }

// lexRefName scans a member name starting at src[start] (just past "A#") and
// returns the name plus the position where lexing resumes. Name characters
// are [A-Za-z0-9 ._-], consumed greedily with two disambiguations so that
// formulas like "A#Sales - A#COGS" still parse as subtraction:
//
//   - a run of spaces continues the name only when followed by a non-hyphen
//     name character (trailing spaces before an operator are trimmed);
//   - a hyphen continues the name only when directly attached on both sides
//     to name characters and not immediately starting a new "A#" reference.
func lexRefName(src string, start int) (name string, next int) {
	n := len(src)
	i, end := start, start
	for i < n {
		c := src[i]
		switch {
		case isNameWord(c):
			i++
			end = i
		case c == ' ':
			j := i
			for j < n && src[j] == ' ' {
				j++
			}
			if j < n && isNameWord(src[j]) {
				i = j // interior spaces: included via src[start:end]
				continue
			}
			return strings.TrimLeft(src[start:end], " "), j // trailing spaces trimmed
		case c == '-' && i == end && i > start &&
			i+1 < n && isNameWord(src[i+1]) &&
			!strings.HasPrefix(src[i+1:], "A#"):
			i++
			end = i
		default:
			return strings.TrimLeft(src[start:end], " "), i
		}
	}
	return strings.TrimLeft(src[start:end], " "), i
}

// lex tokenizes a formula. Errors carry the byte offset of the problem.
func lex(src string) ([]token, error) {
	var toks []token
	n := len(src)
	i := 0
	for i < n {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == 'A' && i+1 < n && src[i+1] == '#':
			name, next := lexRefName(src, i+2)
			if name == "" {
				return nil, fmt.Errorf("offset %d: empty A# reference", i)
			}
			toks = append(toks, token{kind: tkRef, text: name, pos: i})
			i = next
		case isDigit(c) || (c == '.' && i+1 < n && isDigit(src[i+1])):
			j := i
			for j < n && isDigit(src[j]) {
				j++
			}
			if j < n && src[j] == '.' {
				j++
				for j < n && isDigit(src[j]) {
					j++
				}
			}
			if j < n && (src[j] == 'e' || src[j] == 'E') {
				k := j + 1
				if k < n && (src[k] == '+' || src[k] == '-') {
					k++
				}
				if k < n && isDigit(src[k]) {
					for k < n && isDigit(src[k]) {
						k++
					}
					j = k
				}
			}
			text := src[i:j]
			v, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, fmt.Errorf("offset %d: invalid number %q: %w", i, text, err)
			}
			toks = append(toks, token{kind: tkNum, text: text, val: v, pos: i})
			i = j
		case isAlpha(c) || c == '_':
			j := i + 1
			for j < n && (isAlnum(src[j]) || src[j] == '_') {
				j++
			}
			toks = append(toks, token{kind: tkIdent, text: src[i:j], pos: i})
			i = j
		default:
			var kind tokKind
			var text string
			switch c {
			case '+':
				kind, text = tkPlus, "+"
			case '-':
				kind, text = tkMinus, "-"
			case '*':
				kind, text = tkStar, "*"
			case '/':
				kind, text = tkSlash, "/"
			case '(':
				kind, text = tkLParen, "("
			case ')':
				kind, text = tkRParen, ")"
			case ',':
				kind, text = tkComma, ","
			case '<', '>':
				kind, text = tkCmp, string(c)
				if i+1 < n && src[i+1] == '=' {
					text += "="
				}
			case '=', '!':
				if i+1 < n && src[i+1] == '=' {
					kind, text = tkCmp, string(c)+"="
				} else {
					return nil, fmt.Errorf("offset %d: unexpected character %q (did you mean %q?)",
						i, string(c), string(c)+"=")
				}
			default:
				return nil, fmt.Errorf("offset %d: unexpected character %q", i, string(c))
			}
			toks = append(toks, token{kind: kind, text: text, pos: i})
			i += len(text)
		}
	}
	toks = append(toks, token{kind: tkEOF, pos: n})
	return toks, nil
}

// ---- parser ----

type parser struct {
	toks []token
	pos  int
}

func (p *parser) peek() token { return p.toks[p.pos] }

func (p *parser) next() token {
	t := p.toks[p.pos]
	if t.kind != tkEOF {
		p.pos++
	}
	return t
}

// errUnexpected formats an "unexpected token" error; comparison operators get
// a dedicated message because they are only legal as IF's first argument.
func errUnexpected(t token, want string) error {
	if t.kind == tkCmp {
		return fmt.Errorf("offset %d: comparison operator %q is only allowed as the first argument of IF",
			t.pos, t.text)
	}
	return fmt.Errorf("offset %d: unexpected %s, want %s", t.pos, t, want)
}

func (p *parser) expect(kind tokKind, want string) (token, error) {
	t := p.next()
	if t.kind != kind {
		return t, errUnexpected(t, want)
	}
	return t, nil
}

// Parse compiles a formula into an Expr per SPEC §6:
//
//	expr   := term (('+'|'-') term)*
//	term   := factor (('*'|'/') factor)*
//	factor := NUMBER | REF | FUNC '(' expr (',' expr)* ')' | '(' expr ')' | '-' factor
//
// FUNC is one of ABS, MIN, MAX, IF; IF's first argument must be a comparison
// (expr < <= > >= == != expr), and comparisons are illegal anywhere else.
func Parse(src string) (*Expr, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, fmt.Errorf("calc: parse %q: %w", src, err)
	}
	p := &parser{toks: toks}
	if p.peek().kind == tkEOF {
		return nil, fmt.Errorf("calc: parse %q: empty expression", src)
	}
	root, err := p.parseExpr()
	if err != nil {
		return nil, fmt.Errorf("calc: parse %q: %w", src, err)
	}
	if t := p.peek(); t.kind != tkEOF {
		return nil, fmt.Errorf("calc: parse %q: %w", src, errUnexpected(t, "end of expression"))
	}
	return &Expr{root: root, refs: collectRefs(root)}, nil
}

func (p *parser) parseExpr() (*node, error) {
	left, err := p.parseTerm()
	if err != nil {
		return nil, err
	}
	for {
		var kind nodeKind
		switch p.peek().kind {
		case tkPlus:
			kind = nAdd
		case tkMinus:
			kind = nSub
		default:
			return left, nil
		}
		p.next()
		right, err := p.parseTerm()
		if err != nil {
			return nil, err
		}
		left = &node{kind: kind, args: []*node{left, right}}
	}
}

func (p *parser) parseTerm() (*node, error) {
	left, err := p.parseFactor()
	if err != nil {
		return nil, err
	}
	for {
		var kind nodeKind
		switch p.peek().kind {
		case tkStar:
			kind = nMul
		case tkSlash:
			kind = nDiv
		default:
			return left, nil
		}
		p.next()
		right, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		left = &node{kind: kind, args: []*node{left, right}}
	}
}

func (p *parser) parseFactor() (*node, error) {
	t := p.next()
	switch t.kind {
	case tkMinus:
		f, err := p.parseFactor()
		if err != nil {
			return nil, err
		}
		return &node{kind: nNeg, args: []*node{f}}, nil
	case tkNum:
		return &node{kind: nNum, val: t.val}, nil
	case tkRef:
		return &node{kind: nRef, name: t.text}, nil
	case tkLParen:
		e, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tkRParen, `")" to close "("`); err != nil {
			return nil, err
		}
		return e, nil
	case tkIdent:
		return p.parseCall(t)
	default:
		return nil, errUnexpected(t, "a number, A# reference, function call, parenthesized expression, or unary minus")
	}
}

// parseCall parses FUNC '(' args ')' for ABS, MIN, MAX and IF; fn is the
// already-consumed identifier token.
func (p *parser) parseCall(fn token) (*node, error) {
	name := fn.text
	switch name {
	case "ABS", "MIN", "MAX", "IF":
	default:
		return nil, fmt.Errorf("offset %d: unknown function %q (want ABS, MIN, MAX or IF)", fn.pos, name)
	}
	if _, err := p.expect(tkLParen, fmt.Sprintf(`"(" after %s`, name)); err != nil {
		return nil, err
	}
	var args []*node
	if name == "IF" {
		cond, err := p.parseCond()
		if err != nil {
			return nil, err
		}
		args = append(args, cond)
		for i := 0; i < 2; i++ {
			if _, err := p.expect(tkComma, `"," between IF arguments`); err != nil {
				return nil, err
			}
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
		}
	} else {
		first, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		args = append(args, first)
		for p.peek().kind == tkComma {
			p.next()
			a, err := p.parseExpr()
			if err != nil {
				return nil, err
			}
			args = append(args, a)
		}
	}
	if _, err := p.expect(tkRParen, fmt.Sprintf(`")" to close %s(`, name)); err != nil {
		return nil, err
	}
	if name == "ABS" && len(args) != 1 {
		return nil, fmt.Errorf("offset %d: ABS takes exactly 1 argument, got %d", fn.pos, len(args))
	}
	return &node{kind: nCall, name: name, args: args}, nil
}

// parseCond parses IF's first argument: expr CMP expr. The comparison
// operator is mandatory.
func (p *parser) parseCond() (*node, error) {
	lhs, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	t := p.next()
	if t.kind != tkCmp {
		return nil, fmt.Errorf("offset %d: IF condition must be a comparison (expr < <= > >= == != expr), got %s",
			t.pos, t)
	}
	rhs, err := p.parseExpr()
	if err != nil {
		return nil, err
	}
	return &node{kind: nCmp, name: t.text, args: []*node{lhs, rhs}}, nil
}
