package deadcode

import (
	"slices"
	"sync/atomic"

	"github.com/nukilabs/transform/internal/cfg"
	"github.com/t14raptor/go-fast/ast"
	"github.com/t14raptor/go-fast/ast/ext"
	scanner "github.com/t14raptor/go-fast/parser/scanner/token"
	"github.com/t14raptor/go-fast/resolver"
)

// Eliminate removes dead code from the AST.
func Eliminate(p ast.VisitableNode, resolve bool) {
	if resolve {
		resolver.Resolve(p)
	}

	visitor := &treeShaker{changed: true}
	visitor.V = visitor
	for visitor.changed {
		visitor.changed = false
		p.VisitWith(visitor)
	}
}

type treeShaker struct {
	ast.NoopVisitor
	changed bool

	data     data
	bindings map[ast.Id]struct{}

	remove atomic.Bool
}

func (ts *treeShaker) CanDropBinding(id ast.Id, isVar bool) bool {
	if name, ok := ts.data.usedNames[id]; ok {
		return name.Usage == 0 && name.Assign == 0
	}
	return true
}

func (ts *treeShaker) CanDropAssignmentTo(id ast.Id, isVar bool) bool {
	if _, ok := ts.bindings[id]; ok {
		if name, ok := ts.data.usedNames[id]; ok {
			return name.Usage == 0
		}
	}
	return false
}

func (ts *treeShaker) VisitExpressions(n *ast.Expressions) {
	for i := len(*n) - 1; i >= 0; i-- {
		(*n)[i].VisitWith(ts)

		if ts.remove.CompareAndSwap(true, false) {
			*n = slices.Delete(*n, i, i+1)
		}
	}
}

func (ts *treeShaker) VisitStatements(n *ast.Statements) {
	for i := len(*n) - 1; i >= 0; i-- {
		(*n)[i].VisitWith(ts)

		if ts.remove.CompareAndSwap(true, false) {
			*n = slices.Delete(*n, i, i+1)
			continue
		}

		switch (*n)[i].Kind() {
		case ast.StmtEmpty:
			*n = slices.Delete(*n, i, i+1)
		case ast.StmtBlock:
			if len((*n)[i].MustBlock().List) == 0 {
				*n = slices.Delete(*n, i, i+1)
			}
		}
	}
}

func (ts *treeShaker) VisitAssignExpression(n *ast.AssignExpression) {
	n.VisitChildrenWith(ts)

	if ident, ok := n.Left.Ident(); ok {
		if ts.CanDropAssignmentTo(ident.ToId(), false) && !ext.MayHaveSideEffects(n.Right) {
			ts.changed = true
			ts.remove.Store(true)
		}
	}
}

func (ts *treeShaker) VisitFunctionDeclaration(n *ast.FunctionDeclaration) {
	n.VisitChildrenWith(ts)

	if ts.CanDropBinding(n.Function.Name.ToId(), true) {
		ts.changed = true
		ts.remove.Store(true)
	}
}

func (ts *treeShaker) VisitClassDeclaration(n *ast.ClassDeclaration) {
	n.VisitChildrenWith(ts)

	if ts.CanDropBinding(n.Class.Name.ToId(), false) {
		if n.Class.SuperClass != nil && ext.MayHaveSideEffects(n.Class.SuperClass) {
			return
		}

		if slices.ContainsFunc(n.Class.Body, func(elem ast.ClassElement) bool {
			switch elem.Kind() {
			case ast.ClassElemMethod:
				return elem.MustMethod().Computed
			case ast.ClassElemField:
				field := elem.MustField()
				return field.Computed || (field.Initializer != nil && ext.MayHaveSideEffects(field.Initializer))
			case ast.ClassElemStaticBlock:
				return true
			default:
				return false
			}
		}) {
			return
		}

		ts.changed = true
		ts.remove.Store(true)
	}
}

func (ts *treeShaker) VisitExpression(n *ast.Expression) {
	n.VisitChildrenWith(ts)

	if logExpr, ok := n.Logical(); ok {
		switch logExpr.Operator {
		case ast.LogicalAnd:
			if val := ext.AsPureBool(logExpr.Left); val.Known() && !val.Val() {
				*n = *logExpr.Left
				ts.changed = true
			}
		case ast.LogicalOr:
			if val := ext.AsPureBool(logExpr.Left); val.Known() && val.Val() {
				*n = *logExpr.Left
				ts.changed = true
			}
		}
	}
}

func (ts *treeShaker) VisitStatement(n *ast.Statement) {
	n.VisitChildrenWith(ts)

	if varDecl, ok := n.VarDecl(); ok {
		if len(varDecl.List) == 0 {
			ts.remove.Store(true)
		} else {
			// If all name is droppable, do so.
			if slices.ContainsFunc(varDecl.List, func(v ast.VariableDeclarator) bool {
				if ident, ok := v.Target.Ident(); ok {
					return !ts.CanDropBinding(ident.ToId(), varDecl.Token == scanner.Var)
				}
				return true
			}) {
				return
			}

			var exprs []ast.Expression
			for _, v := range varDecl.List {
				if v.Initializer != nil {
					exprs = append(exprs, *v.Initializer)
				}
			}

			if len(exprs) == 0 {
				*n = ast.NewEmptyStmt(&ast.EmptyStatement{})
			} else if len(exprs) == 1 {
				*n = ast.NewExpressionStmt(&ast.ExpressionStatement{Expression: &exprs[0]})
			} else {
				seq := &ast.SequenceExpression{Sequence: exprs}
				seqExpr := ast.NewSequenceExpr(seq)
				*n = ast.NewExpressionStmt(&ast.ExpressionStatement{Expression: &seqExpr})
			}
		}
	}
}

func (ts *treeShaker) VisitUnaryExpression(n *ast.UnaryExpression) {
	if n.Operator == ast.UnaryDelete {
		return
	}
	n.VisitChildrenWith(ts)
}

func (ts *treeShaker) VisitVariableDeclaration(n *ast.VariableDeclaration) {
	for i := len(n.List) - 1; i >= 0; i-- {
		n.List[i].VisitWith(ts)

		if ident, ok := n.List[i].Target.Ident(); ok {
			canDrop := true
			if n.List[i].Initializer != nil {
				canDrop = !ext.MayHaveSideEffects(n.List[i].Initializer)
			}
			if canDrop && ts.CanDropBinding(ident.ToId(), n.Token == scanner.Var) {
				ts.changed = true
				n.List = slices.Delete(n.List, i, i+1)
			}
		}
	}
}

func (ts *treeShaker) VisitProgram(n *ast.Program) {
	if len(ts.bindings) == 0 {
		ts.bindings = collectDeclarations(n)
	}

	data := data{
		usedNames: make(map[ast.Id]varInfo),
		graph:     cfg.NewDirectedGraph[ast.Id, varInfo](),
		entries:   make(map[ast.Id]struct{}),
	}

	analyzer := &analyzer{
		data:  &data,
		scope: &scope{},
	}
	analyzer.V = analyzer
	n.VisitWith(analyzer)

	data.SubtractCycles()
	ts.data = data

	n.VisitChildrenWith(ts)
}
