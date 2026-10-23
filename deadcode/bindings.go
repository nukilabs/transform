package deadcode

import "github.com/t14raptor/go-fast/ast"

type bindingCollector struct {
	ast.NoopVisitor
	only     *ast.ScopeContext
	bindings map[ast.Id]struct{}
	// inAssignTarget is set while visiting the target of an assignment or a
	// for-in/of head without a declaration. Identifiers there refer to
	// existing bindings (or implicit globals) and do not declare anything.
	inAssignTarget bool
}

func (v *bindingCollector) add(id ast.Id) {
	if v.only != nil && *v.only != id.ScopeContext {
		return
	}
	v.bindings[id] = struct{}{}
}

func (v *bindingCollector) VisitClassLiteral(n *ast.ClassLiteral) {
	n.VisitChildrenWith(v)
	v.add(n.Name.ToId())
}

func (v *bindingCollector) VisitFunctionDeclaration(n *ast.FunctionDeclaration) {
	n.VisitChildrenWith(v)
	v.add(n.Function.Name.ToId())
}

func (v *bindingCollector) VisitPattern(n *ast.Pattern) {
	n.VisitChildrenWith(v)
	if ident, ok := n.Identifier(); ok && !v.inAssignTarget {
		v.add(ident.ToId())
	}
}

func (v *bindingCollector) VisitAssignExpression(n *ast.AssignExpression) {
	v.visitTarget(n.Left)
	n.Right.VisitWith(v)
}

func (v *bindingCollector) VisitForInto(n *ast.ForInto) {
	if pat, ok := n.Pattern(); ok {
		v.visitTarget(pat)
		return
	}
	n.VisitChildrenWith(v)
}

// VisitExpression clears inAssignTarget so that default values and computed
// keys inside an assignment target are still searched for declarations.
func (v *bindingCollector) VisitExpression(n *ast.Expression) {
	old := v.inAssignTarget
	v.inAssignTarget = false
	n.VisitChildrenWith(v)
	v.inAssignTarget = old
}

func (v *bindingCollector) visitTarget(n *ast.Pattern) {
	old := v.inAssignTarget
	v.inAssignTarget = true
	n.VisitWith(v)
	v.inAssignTarget = old
}

// collectDeclarations collects binding identifiers.
func collectDeclarations(n ast.VisitableNode) map[ast.Id]struct{} {
	visitor := &bindingCollector{
		bindings: make(map[ast.Id]struct{}),
	}
	visitor.V = visitor
	n.VisitWith(visitor)
	return visitor.bindings
}

// collectDeclarationsInScope collects binding if they are in the given scope.
func collectDeclarationsInScope(n ast.VisitableNode, scope ast.ScopeContext) map[ast.Id]struct{} {
	visitor := &bindingCollector{
		only:     &scope,
		bindings: make(map[ast.Id]struct{}),
	}
	visitor.V = visitor
	n.VisitWith(visitor)
	return visitor.bindings
}

type destructingCollector struct {
	ast.NoopVisitor
	bindings map[ast.Id]struct{}
}

func (v *destructingCollector) VisitExpression(n *ast.Expression) {}
func (v *destructingCollector) VisitIdentifier(n *ast.Identifier) {
	v.bindings[n.ToId()] = struct{}{}
}

func collectIdentifiers(n ast.VisitableNode) map[ast.Id]struct{} {
	visitor := &destructingCollector{
		bindings: make(map[ast.Id]struct{}),
	}
	visitor.V = visitor
	n.VisitWith(visitor)
	return visitor.bindings
}
