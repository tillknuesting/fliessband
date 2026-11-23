package metrics

import (
	"go/ast"
	"go/scanner"
	"go/token"
	"math"
)

// lineInfo marks which lines of a file hold code and which hold comments.
type lineInfo struct {
	total   int
	code    []bool // index = line number
	comment []bool
}

func scanLines(src []byte) lineInfo {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	total := 1
	for _, c := range src {
		if c == '\n' {
			total++
		}
	}
	if len(src) > 0 && src[len(src)-1] == '\n' {
		total--
	}
	li := lineInfo{total: total, code: make([]bool, total+2), comment: make([]bool, total+2)}
	var s scanner.Scanner
	s.Init(file, src, nil, scanner.ScanComments)
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		if tok == token.SEMICOLON && lit == "\n" {
			continue
		}
		start := file.Line(pos)
		end := start
		if tok == token.COMMENT || tok == token.STRING {
			end = file.Line(pos + token.Pos(len(lit)) - 1)
		}
		for l := start; l <= end && l < len(li.code); l++ {
			if tok == token.COMMENT {
				li.comment[l] = true
			} else {
				li.code[l] = true
			}
		}
	}
	return li
}

func (li lineInfo) counts(from, to int) (code, comment, blank int) {
	for l := from; l <= to && l < len(li.code); l++ {
		switch {
		case li.code[l]:
			code++
		case li.comment[l]:
			comment++
		default:
			blank++
		}
	}
	return
}

// cyclomatic returns McCabe complexity: 1 + decision points (gocyclo rules).
func cyclomatic(fn ast.Node) int {
	c := 1
	ast.Inspect(fn, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			c++
		case *ast.CaseClause:
			if n.List != nil {
				c++
			}
		case *ast.CommClause:
			if n.Comm != nil {
				c++
			}
		case *ast.BinaryExpr:
			if n.Op == token.LAND || n.Op == token.LOR {
				c++
			}
		}
		return true
	})
	return c
}

// cognitive returns the cognitive complexity of a function following the
// SonarSource definition: increments for breaks in linear flow, weighted by
// nesting depth.
func cognitive(fn *ast.FuncDecl) int {
	w := &cogWalker{name: fn.Name.Name, elseIfs: map[*ast.IfStmt]bool{}, seen: map[*ast.BinaryExpr]bool{}}
	w.walk(fn.Body)
	return w.complexity
}

type cogWalker struct {
	name       string
	complexity int
	nesting    int
	elseIfs    map[*ast.IfStmt]bool
	seen       map[*ast.BinaryExpr]bool
}

func (w *cogWalker) nested(n ast.Node) {
	w.nesting++
	w.walk(n)
	w.nesting--
}

func (w *cogWalker) walk(n ast.Node) {
	if n == nil {
		return
	}
	switch n := n.(type) {
	case *ast.IfStmt:
		if !w.elseIfs[n] {
			w.complexity += 1 + w.nesting
		}
		w.walk(n.Init)
		w.walk(n.Cond)
		w.nested(n.Body)
		switch e := n.Else.(type) {
		case *ast.IfStmt:
			w.elseIfs[e] = true
			w.complexity++
			w.walk(e)
		case nil:
		default:
			w.complexity++
			w.nested(e)
		}
	case *ast.ForStmt:
		w.complexity += 1 + w.nesting
		w.walk(n.Init)
		w.walk(n.Cond)
		w.walk(n.Post)
		w.nested(n.Body)
	case *ast.RangeStmt:
		w.complexity += 1 + w.nesting
		w.walk(n.X)
		w.nested(n.Body)
	case *ast.SwitchStmt:
		w.complexity += 1 + w.nesting
		w.walk(n.Init)
		w.walk(n.Tag)
		w.nested(n.Body)
	case *ast.TypeSwitchStmt:
		w.complexity += 1 + w.nesting
		w.walk(n.Init)
		w.walk(n.Assign)
		w.nested(n.Body)
	case *ast.SelectStmt:
		w.complexity += 1 + w.nesting
		w.nested(n.Body)
	case *ast.FuncLit:
		w.nested(n.Body)
	case *ast.BranchStmt:
		if n.Tok == token.GOTO || (n.Label != nil && n.Tok != token.FALLTHROUGH) {
			w.complexity++
		}
	case *ast.BinaryExpr:
		if (n.Op == token.LAND || n.Op == token.LOR) && !w.seen[n] {
			var ops []token.Token
			var leaves []ast.Expr
			w.flatten(n, &ops, &leaves)
			w.complexity++
			for i := 1; i < len(ops); i++ {
				if ops[i] != ops[i-1] {
					w.complexity++
				}
			}
			for _, l := range leaves {
				w.walk(l)
			}
			return
		}
		w.walk(n.X)
		w.walk(n.Y)
	case *ast.CallExpr:
		if id, ok := n.Fun.(*ast.Ident); ok && id.Name == w.name {
			w.complexity++
		}
		w.walk(n.Fun)
		for _, a := range n.Args {
			w.walk(a)
		}
	default:
		ast.Inspect(n, func(c ast.Node) bool {
			if c == n {
				return true
			}
			if c != nil {
				w.walk(c)
			}
			return false
		})
	}
}

func (w *cogWalker) flatten(e ast.Expr, ops *[]token.Token, leaves *[]ast.Expr) {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = p.X
	}
	b, ok := e.(*ast.BinaryExpr)
	if !ok || (b.Op != token.LAND && b.Op != token.LOR) {
		*leaves = append(*leaves, e)
		return
	}
	w.seen[b] = true
	w.flatten(b.X, ops, leaves)
	*ops = append(*ops, b.Op)
	w.flatten(b.Y, ops, leaves)
}

// Halstead holds Halstead software science measures for one function.
type Halstead struct {
	DistinctOperators int     `json:"n1"`
	DistinctOperands  int     `json:"n2"`
	Operators         int     `json:"N1"`
	Operands          int     `json:"N2"`
	Volume            float64 `json:"volume"`
	Difficulty        float64 `json:"difficulty"`
	Effort            float64 `json:"effort"`
}

func halstead(src []byte) Halstead {
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(src))
	var s scanner.Scanner
	s.Init(file, src, nil, 0)
	ops, opnds := map[string]int{}, map[string]int{}
	var h Halstead
	for {
		_, tok, lit := s.Scan()
		if tok == token.EOF {
			break
		}
		switch {
		case tok == token.SEMICOLON && lit == "\n":
		case tok.IsOperator() || tok.IsKeyword():
			ops[tok.String()]++
			h.Operators++
		case tok.IsLiteral():
			opnds[lit]++
			h.Operands++
		}
	}
	h.DistinctOperators, h.DistinctOperands = len(ops), len(opnds)
	n := float64(h.DistinctOperators + h.DistinctOperands)
	N := float64(h.Operators + h.Operands)
	if n > 1 {
		h.Volume = N * math.Log2(n)
	}
	if h.DistinctOperands > 0 {
		h.Difficulty = float64(h.DistinctOperators) / 2 * float64(h.Operands) / float64(h.DistinctOperands)
	}
	h.Effort = h.Difficulty * h.Volume
	return h
}

// maintainabilityIndex is the 0-100 normalised index used by Visual Studio:
// (171 - 5.2 ln V - 0.23 CC - 16.2 ln LOC) * 100 / 171, clamped at 0.
func maintainabilityIndex(volume float64, cc, loc int) float64 {
	v := math.Max(volume, 1)
	l := math.Max(float64(loc), 1)
	mi := (171 - 5.2*math.Log(v) - 0.23*float64(cc) - 16.2*math.Log(l)) * 100 / 171
	return math.Max(0, math.Min(100, mi))
}
