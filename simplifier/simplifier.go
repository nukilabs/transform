package simplifier

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/t14raptor/go-fast/resolver"

	"github.com/nukilabs/unicodeid"
	"github.com/t14raptor/go-fast/ast"
	"github.com/t14raptor/go-fast/ast/ext"
)

var asciiStart, asciiContinue [128]bool

func init() {
	for i := range 128 {
		if i >= 'a' && i <= 'z' || i >= 'A' && i <= 'Z' || i == '$' || i == '_' {
			asciiStart[i] = true
			asciiContinue[i] = true
		}
		if i >= '0' && i <= '9' {
			asciiContinue[i] = true
		}
	}
}

func isIdentifierStart(chr rune) bool {
	if chr < utf8.RuneSelf {
		return asciiStart[chr]
	}
	return unicodeid.IsIDStartUnicode(chr)
}

func isIdentifierPart(chr rune) bool {
	if chr < utf8.RuneSelf {
		return asciiContinue[chr]
	}
	return unicodeid.IsIDContinueUnicode(chr)
}

func isIdentifier(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if i == 0 && !isIdentifierStart(r) || i > 0 && !isIdentifierPart(r) {
			return false
		}
	}
	return true
}

type simplifier struct {
	ast.NoopVisitor

	changed       bool
	isArgOfUpdate bool
	isModifying   bool
	inCallee      bool
}

func (s *simplifier) optimizeMemberExpression(expr *ast.Expression) {
	memExpr, ok := expr.Member()
	if !ok {
		return
	}

	type Len struct{}
	type Index float64
	type IndexStr string

	var op any
	switch memExpr.Property.Kind() {
	case ast.MemPropIdent:
		prop := memExpr.Property.MustIdent()
		if !memExpr.Object.IsObjLit() && prop.Name == "length" {
			op = Len{}
		} else if s.inCallee {
			return
		} else {
			op = IndexStr(prop.Name)
		}
	case ast.MemPropComputed:
		prop := memExpr.Property.MustComputed()
		if numLit, ok := prop.Expr.NumLit(); ok {
			op = Index(numLit.Value)
		} else if sv := ext.AsPureString(prop.Expr); sv.Known() {
			if !memExpr.Object.IsObjLit() && sv.Val() == "length" {
				op = Len{}
			} else if n, err := strconv.ParseFloat(sv.Val(), 64); err == nil {
				op = Index(n)
			} else {
				op = IndexStr(sv.Val())
			}
		} else {
			return
		}
	}

	switch memExpr.Object.Kind() {
	case ast.ExprStrLit:
		obj := memExpr.Object.MustStrLit()
		switch op := op.(type) {
		case Len:
			s.changed = true
			*expr = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: memExpr.Idx0(), Value: float64(utf8.RuneCountInString(obj.Value))})
		case Index:
			idx := float64(op)
			if _, frac := math.Modf(float64(idx)); frac != 0.0 || idx < 0.0 || int(idx) >= len(obj.Value) {
				return
			}
			var b strings.Builder
			for _, c := range obj.Value {
				surrogates := utf16.Encode([]rune{c})
				if len(surrogates) == 2 {
					fmt.Fprintf(&b, "\\u%04X\\u%04X", surrogates[0], surrogates[1])
				} else {
					b.WriteRune(c)
				}
			}
			input := b.String()
			value, ok := nthChar(input, int(idx))
			if !ok {
				return
			}
			s.changed = true
			raw := fmt.Sprintf("\"%s\"", value)
			*expr = ast.NewStrLitExpr(&ast.StringLiteral{Idx: memExpr.Idx0(), Value: value, Raw: &raw})
		case IndexStr:
			if !ext.IsStringSymbol(string(op)) {
				*expr = ast.NewIdentExpr(&ast.Identifier{Idx: memExpr.Idx0(), Name: "undefined"})
			}
		}

	case ast.ExprArrLit:
		obj := memExpr.Object.MustArrLit()
		if _, ok := op.(IndexStr); !ok && (s.inCallee || s.isModifying) {
			return
		}
		if slices.ContainsFunc(obj.Value, func(e ast.Expression) bool {
			return e.IsSpread()
		}) {
			return
		}

		switch op := op.(type) {
		case Len:
			if slices.ContainsFunc(obj.Value, func(e ast.Expression) bool {
				return ext.MayHaveSideEffects(&e)
			}) {
				return
			}
			s.changed = true
			*expr = ast.NewNumLitExpr(&ast.NumberLiteral{Value: float64(len(obj.Value))})
		case Index:
			idx := int(op)
			if _, frac := math.Modf(float64(idx)); frac != 0.0 || idx < 0 || idx >= len(obj.Value) {
				return
			}
			if slices.ContainsFunc(obj.Value[idx+1:], func(e ast.Expression) bool {
				return ext.MayHaveSideEffects(&e)
			}) {
				return
			}
			s.changed = true
			before := obj.Value[:idx]
			e := obj.Value[idx]
			after := obj.Value[idx+1:]
			var v ast.Expression
			if e.IsNone() {
				v = ast.NewUnaryExpr(&ast.UnaryExpression{
					Idx: memExpr.Idx0(), Operator: ast.UnaryVoid,
					Operand: ptrExpr(ast.NewNumLitExpr(&ast.NumberLiteral{Idx: memExpr.Idx0(), Value: 0.0})),
				})
			} else {
				v = e
			}
			var exprs []ast.Expression
			for _, elem := range before {
				ext.ExtractSideEffectsTo(&exprs, &elem)
			}
			val := v
			for _, elem := range after {
				ext.ExtractSideEffectsTo(&exprs, &elem)
			}
			if exprs == nil {
				*expr = ast.NewSequenceExpr(&ast.SequenceExpression{
					Sequence: []ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}), val},
				})
				return
			}
			exprs = append(exprs, val)
			*expr = ast.NewSequenceExpr(&ast.SequenceExpression{Sequence: exprs})
		case IndexStr:
			if len(obj.Value) == 0 && !ext.IsArraySymbol(string(op)) {
				*expr = ast.NewIdentExpr(&ast.Identifier{Idx: memExpr.Idx0(), Name: "undefined"})
			}
		}

	case ast.ExprObjLit:
		obj := memExpr.Object.MustObjLit()
		if s.inCallee || s.isModifying {
			return
		}
		var key string
		switch op := op.(type) {
		case Index:
			key = strconv.FormatFloat(float64(op), 'f', -1, 64)
		case IndexStr:
			if op != "yield" && ext.IsLiteral(&obj.Value) {
				key = string(op)
			}
		}
		v := getKeyValue(obj.Value, key)
		if v == nil {
			return
		}
		s.changed = true
		objExpr := ast.NewObjLitExpr(obj)
		*expr = ext.PreserveEffects(*v, []ast.Expression{objExpr})
	}
}

func (s *simplifier) optimizeBinaryExpression(expr *ast.Expression) {
	binExpr, ok := expr.Binary()
	if !ok {
		return
	}

	tryReplaceBool := func(v bool, left, right *ast.Expression) {
		s.changed = true
		*expr = makeBoolExpr(v, []ast.Expression{*left, *right})
	}
	tryReplaceNum := func(v float64, left, right *ast.Expression) {
		s.changed = true
		var value ast.Expression
		if !math.IsNaN(v) {
			value = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: binExpr.Idx0(), Value: v})
		} else {
			value = ast.NewIdentExpr(&ast.Identifier{Idx: binExpr.Idx0(), Name: "NaN"})
		}
		*expr = ext.PreserveEffects(value, []ast.Expression{*left, *right})
	}

	switch binExpr.Operator {
	case ast.BinaryAddition:
		if ext.IsString(binExpr.Left) || ext.IsArrayLiteral(binExpr.Left) || ext.IsString(binExpr.Right) || ext.IsArrayLiteral(binExpr.Right) {
			l := ext.AsPureString(binExpr.Left)
			r := ext.AsPureString(binExpr.Right)
			if l.Known() && r.Known() {
				s.changed = true
				*expr = ast.NewStrLitExpr(&ast.StringLiteral{Idx: binExpr.Idx0(), Value: l.Val() + r.Val()})
			}
		}
		typ := ext.GetType(expr)
		if typ.Unknown() {
			return
		}
		switch typ.Val().(type) {
		case ext.StringType:
			if !ext.MayHaveSideEffects(binExpr.Left) && !ext.MayHaveSideEffects(binExpr.Right) {
				l := ext.AsPureString(binExpr.Left)
				r := ext.AsPureString(binExpr.Right)
				if l.Known() && r.Known() {
					s.changed = true
					*expr = ast.NewStrLitExpr(&ast.StringLiteral{Idx: binExpr.Idx0(), Value: l.Val() + r.Val()})
				}
			}
		case ext.BoolType, ext.NullType, ext.NumberType, ext.UndefinedType:
			if v := s.performArithmeticOp(ast.BinaryAddition, binExpr.Left, binExpr.Right); v.Known() {
				tryReplaceNum(v.Val(), binExpr.Left, binExpr.Right)
			}
		}
	case ast.BinaryInstanceof:
		if isNonObj(binExpr.Left) {
			s.changed = true
			*expr = makeBoolExpr(false, []ast.Expression{*binExpr.Right})
			return
		}
		if isObj(binExpr.Left) && ext.IsGlobalRefTo(binExpr.Right, "Object") {
			s.changed = true
			*expr = makeBoolExpr(true, []ast.Expression{*binExpr.Left})
		}
	case ast.BinarySubtraction, ast.BinaryDivision, ast.BinaryRemainder, ast.BinaryExponential:
		if v := s.performArithmeticOp(binExpr.Operator, binExpr.Left, binExpr.Right); v.Known() {
			tryReplaceNum(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryShiftLeft, ast.BinaryShiftRight, ast.BinaryUnsignedShiftRight:
		tryFoldShift := func(op ast.BinaryOperator, left, right *ast.Expression) (float64, bool) {
			if !left.IsNumLit() || !right.IsNumLit() {
				return 0, false
			}
			lv := ext.AsPureNumber(left)
			rv := ext.AsPureNumber(right)
			if lv.Unknown() || rv.Unknown() {
				return 0, false
			}
			switch op {
			case ast.BinaryShiftLeft:
				return float64(toInt32(lv.Val()) << (toUint32(rv.Val()) & 0x1f)), true
			case ast.BinaryShiftRight:
				return float64(toInt32(lv.Val()) >> (toUint32(rv.Val()) & 0x1f)), true
			case ast.BinaryUnsignedShiftRight:
				return float64(toUint32(lv.Val()) >> (toUint32(rv.Val()) & 0x1f)), true
			}
			return 0, false
		}
		if v, ok := tryFoldShift(binExpr.Operator, binExpr.Left, binExpr.Right); ok {
			tryReplaceNum(v, binExpr.Left, binExpr.Right)
		}
	case ast.BinaryMultiplication, ast.BinaryBitwiseAnd, ast.BinaryBitwiseOR, ast.BinaryBitwiseXOR:
		if v := s.performArithmeticOp(binExpr.Operator, binExpr.Left, binExpr.Right); v.Known() {
			tryReplaceNum(v.Val(), binExpr.Left, binExpr.Right)
		}
		if binExpr2, ok := binExpr.Left.Binary(); ok && binExpr2.Operator == binExpr.Operator {
			if v := s.performArithmeticOp(binExpr.Operator, binExpr2.Right, binExpr.Right); v.Known() {
				var valExpr ast.Expression
				if !math.IsNaN(v.Val()) {
					valExpr = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: binExpr.Idx0(), Value: v.Val()})
				} else {
					valExpr = ast.NewIdentExpr(&ast.Identifier{Idx: binExpr.Idx0(), Name: "NaN"})
				}
				s.changed = true
				*binExpr.Left = *binExpr2.Left
				*binExpr.Right = valExpr
			}
		}
	case ast.BinaryLessThan:
		if v := s.performAbstractRelCmp(binExpr.Left, binExpr.Right, false); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryGreaterThan:
		if v := s.performAbstractRelCmp(binExpr.Right, binExpr.Left, false); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Right, binExpr.Left)
		}
	case ast.BinaryLessEqualThan:
		if v := s.performAbstractRelCmp(binExpr.Right, binExpr.Left, true).Not(); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Right, binExpr.Left)
		}
	case ast.BinaryGreaterEqualThan:
		if v := s.performAbstractRelCmp(binExpr.Left, binExpr.Right, true).Not(); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryEquality:
		if v := s.performAbstractEqCmp(binExpr.Left, binExpr.Right); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryInequality:
		if v := s.performAbstractEqCmp(binExpr.Left, binExpr.Right).Not(); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryStrictEquality:
		if v := s.performStrictEqCmp(binExpr.Left, binExpr.Right); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	case ast.BinaryStrictInequality:
		if v := s.performStrictEqCmp(binExpr.Left, binExpr.Right).Not(); v.Known() {
			tryReplaceBool(v.Val(), binExpr.Left, binExpr.Right)
		}
	}
}

func (s *simplifier) optimizeLogicalExpression(expr *ast.Expression) {
	logExpr, ok := expr.Logical()
	if !ok {
		return
	}

	val, _ := ext.CastToBool(logExpr.Left)
	if val.Unknown() {
		return
	}

	var node ast.Expression
	if logExpr.Operator == ast.LogicalAnd {
		if val.Val() {
			node = *logExpr.Right
		} else {
			s.changed = true
			*expr = *logExpr.Left
			return
		}
	} else {
		if val.Val() {
			s.changed = true
			*expr = *logExpr.Left
			return
		} else {
			node = *logExpr.Right
		}
	}
	if !ext.MayHaveSideEffects(logExpr.Left) {
		s.changed = true
		if directnessMaters(&node) {
			*expr = ast.NewSequenceExpr(&ast.SequenceExpression{
				Sequence: []ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}), node},
			})
		} else {
			*expr = node
		}
	} else {
		s.changed = true
		seq := &ast.SequenceExpression{Sequence: []ast.Expression{*logExpr.Left, node}}
		seqExpr := ast.NewSequenceExpr(seq)
		seqExpr.VisitWith(s)
		*expr = seqExpr
	}
}

func (s *simplifier) tryFoldTypeOf(expr *ast.Expression) {
	unary, ok := expr.Unary()
	if !ok || unary.Operator != ast.UnaryTypeof {
		return
	}
	var val string
	switch unary.Operand.Kind() {
	case ast.ExprFuncLit:
		val = "function"
	case ast.ExprStrLit:
		val = "string"
	case ast.ExprNumLit:
		val = "number"
	case ast.ExprBoolLit:
		val = "boolean"
	case ast.ExprNullLit, ast.ExprObjLit, ast.ExprArrLit:
		val = "object"
	case ast.ExprUnary:
		if unary.Operand.MustUnary().Operator == ast.UnaryVoid {
			val = "undefined"
		} else {
			return
		}
	case ast.ExprIdent:
		if unary.Operand.MustIdent().Name == "undefined" {
			val = "undefined"
		} else {
			return
		}
	default:
		return
	}
	s.changed = true
	*expr = ast.NewStrLitExpr(&ast.StringLiteral{Value: val})
}

func (s *simplifier) optimizeUnaryExpression(expr *ast.Expression) {
	unaryExpr, ok := expr.Unary()
	if !ok {
		return
	}
	sideEffects := ext.MayHaveSideEffects(unaryExpr.Operand)

	switch unaryExpr.Operator {
	case ast.UnaryTypeof:
		if !sideEffects {
			s.tryFoldTypeOf(expr)
		}
	case ast.UnaryLogicalNot:
		switch unaryExpr.Operand.Kind() {
		case ast.ExprNumLit:
			return
		case ast.ExprCall:
			if unaryExpr.Operand.MustCall().Callee.IsFuncLit() {
				return
			}
		}
		if val, _ := ext.CastToBool(unaryExpr.Operand); val.Known() {
			s.changed = true
			*expr = makeBoolExpr(val.Not().Val(), []ast.Expression{*unaryExpr.Operand})
		}
	case ast.UnaryPlus:
		if val := ext.AsPureNumber(unaryExpr.Operand); val.Known() {
			s.changed = true
			if math.IsNaN(val.Val()) {
				*expr = ext.PreserveEffects(ast.NewIdentExpr(&ast.Identifier{Idx: unaryExpr.Idx, Name: "NaN"}), []ast.Expression{*unaryExpr.Operand})
				return
			}
			*expr = ext.PreserveEffects(ast.NewNumLitExpr(&ast.NumberLiteral{Idx: unaryExpr.Idx, Value: val.Val()}), []ast.Expression{*unaryExpr.Operand})
		}
	case ast.UnaryNegation:
		switch unaryExpr.Operand.Kind() {
		case ast.ExprIdent:
			switch unaryExpr.Operand.MustIdent().Name {
			case "Infinity":
			case "NaN":
				s.changed = true
				*expr = *unaryExpr.Operand
			}
		case ast.ExprNumLit:
			operand := unaryExpr.Operand.MustNumLit()
			s.changed = true
			*expr = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: operand.Idx, Value: -operand.Value})
		}
	case ast.UnaryVoid:
		if !sideEffects {
			if numLit, ok := unaryExpr.Operand.NumLit(); ok && numLit.Value == 0 {
				return
			}
			s.changed = true
			*unaryExpr.Operand = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: unaryExpr.Operand.Idx0(), Value: 0.0})
		}
	case ast.UnaryBitwiseNot:
		if val := ext.AsPureNumber(unaryExpr.Operand); val.Known() {
			if _, frac := math.Modf(val.Val()); frac == 0.0 {
				s.changed = true
				*expr = ast.NewNumLitExpr(&ast.NumberLiteral{Idx: unaryExpr.Idx, Value: float64(^toInt32(val.Val()))})
			}
		}
	}
}

func (s *simplifier) performArithmeticOp(op ast.BinaryOperator, left, right *ast.Expression) ext.Value[float64] {
	tryReplace := func(v float64) ext.Value[float64] {
		newLen := len(strconv.FormatFloat(v, 'f', -1, 64))
		if right.Idx1() > left.Idx0() {
			origLen := right.Idx1() - right.Idx0() + left.Idx1() - left.Idx0()
			if newLen <= int(origLen)+1 {
				return ext.Known(v)
			}
			return ext.Unknown[float64]()
		}
		return ext.Known(v)
	}

	lv := ext.AsPureNumber(left)
	rv := ext.AsPureNumber(right)
	if (lv.Unknown() && rv.Unknown()) || op == ast.BinaryAddition &&
		(!ext.GetType(left).CastToNumberOnAdd() || !ext.GetType(right).CastToNumberOnAdd()) {
		return ext.Unknown[float64]()
	}

	switch op {
	case ast.BinaryAddition:
		if lv.Known() && rv.Known() {
			return tryReplace(lv.Val() + rv.Val())
		}
		if lv == ext.Known(0.0) {
			return rv
		} else if rv == ext.Known(0.0) {
			return lv
		}
		return ext.Unknown[float64]()
	case ast.BinarySubtraction:
		if lv.Known() && rv.Known() {
			return tryReplace(lv.Val() - rv.Val())
		}
		if lv == ext.Known(0.0) && rv.Known() {
			return ext.Known(-rv.Val())
		}
		if rv == ext.Known(0.0) {
			return lv
		}
		return ext.Unknown[float64]()
	case ast.BinaryMultiplication:
		if lv.Known() && rv.Known() {
			return tryReplace(lv.Val() * rv.Val())
		}
		if lv == ext.Known(1.0) {
			return rv
		}
		if rv == ext.Known(1.0) {
			return lv
		}
		return ext.Unknown[float64]()
	case ast.BinaryDivision:
		if lv.Known() && rv.Known() {
			if rv.Val() == 0.0 {
				return ext.Unknown[float64]()
			}
			return tryReplace(lv.Val() / rv.Val())
		}
		if rv == ext.Known(1.0) {
			return lv
		}
		return ext.Unknown[float64]()
	case ast.BinaryExponential:
		if rv == ext.Known(0.0) {
			return ext.Known(1.0)
		}
		if lv.Known() && rv.Known() {
			return tryReplace(math.Pow(lv.Val(), rv.Val()))
		}
		return ext.Unknown[float64]()
	}

	if lv.Unknown() || rv.Unknown() {
		return ext.Unknown[float64]()
	}

	switch op {
	case ast.BinaryBitwiseAnd:
		return tryReplace(float64(toInt32(lv.Val()) & toInt32(rv.Val())))
	case ast.BinaryBitwiseOR:
		return tryReplace(float64(toInt32(lv.Val()) | toInt32(rv.Val())))
	case ast.BinaryBitwiseXOR:
		return tryReplace(float64(toInt32(lv.Val()) ^ toInt32(rv.Val())))
	case ast.BinaryRemainder:
		if rv.Val() == 0.0 {
			return ext.Unknown[float64]()
		}
		return tryReplace(math.Mod(lv.Val(), rv.Val()))
	}
	return ext.Unknown[float64]()
}

func (s *simplifier) performAbstractRelCmp(left, right *ast.Expression, willNegate bool) ext.BoolValue {
	if l, ok := left.Ident(); ok {
		if r, ok := right.Ident(); ok {
			if !willNegate && l.Name == r.Name && l.ScopeContext == r.ScopeContext {
				return ext.BoolValue{Value: ext.Known(false)}
			}
		}
	}
	if l, ok := left.Unary(); ok && l.Operator == ast.UnaryTypeof {
		if r, ok := right.Unary(); ok && r.Operator == ast.UnaryTypeof {
			if lid, lok := l.Operand.Ident(); lok {
				if rid, rok := r.Operand.Ident(); rok {
					if lid.ToId() == rid.ToId() {
						return ext.BoolValue{Value: ext.Known(false)}
					}
				}
			}
		}
	}

	lt := ext.GetType(left)
	rt := ext.GetType(right)
	if lt.Value == ext.Known[ext.Type](ext.StringType{}) && rt.Value == ext.Known[ext.Type](ext.StringType{}) {
		lv := ext.AsPureString(left)
		rv := ext.AsPureString(right)
		if lv.Known() && rv.Known() {
			if strings.ContainsRune(lv.Val(), '\u000B') || strings.ContainsRune(rv.Val(), '\u000B') {
				return ext.BoolValue{Value: ext.Unknown[bool]()}
			}
			return ext.BoolValue{Value: ext.Known(lv.Val() < rv.Val())}
		}
	}

	lv := ext.AsPureNumber(left)
	rv := ext.AsPureNumber(right)
	if lv.Known() && rv.Known() {
		if math.IsNaN(lv.Val()) || math.IsNaN(rv.Val()) {
			return ext.BoolValue{Value: ext.Known(willNegate)}
		}
		return ext.BoolValue{Value: ext.Known(lv.Val() < rv.Val())}
	}
	return ext.BoolValue{Value: ext.Unknown[bool]()}
}

func (s *simplifier) performAbstractEqCmp(left, right *ast.Expression) ext.BoolValue {
	lt := ext.GetType(left)
	rt := ext.GetType(right)
	if lt.Unknown() || rt.Unknown() {
		return ext.BoolValue{Value: ext.Unknown[bool]()}
	}
	if lt.Val() == rt.Val() {
		return s.performStrictEqCmp(left, right)
	}
	if (lt.Val() == ext.NullType{} && rt.Val() == ext.UndefinedType{}) || (lt.Val() == ext.UndefinedType{} && rt.Val() == ext.NullType{}) {
		return ext.BoolValue{Value: ext.Known(true)}
	}
	if (lt.Val() == ext.NumberType{} && rt.Val() == ext.StringType{}) || (rt.Val() == ext.BoolType{}) {
		rv := ext.AsPureNumber(right)
		if rv.Unknown() {
			return ext.BoolValue{Value: ext.Unknown[bool]()}
		}
		numExpr := ast.NewNumLitExpr(&ast.NumberLiteral{Value: rv.Val()})
		return s.performAbstractEqCmp(left, &numExpr)
	}
	if (lt.Val() == ext.StringType{} && rt.Val() == ext.NumberType{}) || lt.Val() == (ext.BoolType{}) {
		lv := ext.AsPureNumber(left)
		if lv.Unknown() {
			return ext.BoolValue{Value: ext.Unknown[bool]()}
		}
		numExpr := ast.NewNumLitExpr(&ast.NumberLiteral{Value: lv.Val()})
		return s.performAbstractEqCmp(&numExpr, right)
	}
	if (lt.Val() == ext.StringType{} && rt.Val() == ext.ObjectType{}) || (lt.Val() == ext.NumberType{} && rt.Val() == ext.ObjectType{}) ||
		(lt.Val() == ext.ObjectType{} && rt.Val() == ext.StringType{}) || (lt.Val() == ext.ObjectType{} && rt.Val() == ext.NumberType{}) {
		return ext.BoolValue{Value: ext.Unknown[bool]()}
	}
	return ext.BoolValue{Value: ext.Known(false)}
}

func (s *simplifier) performStrictEqCmp(left, right *ast.Expression) ext.BoolValue {
	if ext.IsNaN(left) || ext.IsNaN(right) {
		return ext.BoolValue{Value: ext.Known(false)}
	}
	if l, ok := left.Unary(); ok && l.Operator == ast.UnaryTypeof {
		if r, ok := right.Unary(); ok && r.Operator == ast.UnaryTypeof {
			if lid, lok := l.Operand.Ident(); lok {
				if rid, rok := r.Operand.Ident(); rok {
					if lid.ToId() == rid.ToId() {
						return ext.BoolValue{Value: ext.Known(true)}
					}
				}
			}
		}
	}
	lt := ext.GetType(left)
	rt := ext.GetType(right)
	if lt.Unknown() || rt.Unknown() {
		return ext.BoolValue{Value: ext.Unknown[bool]()}
	}
	if lt.Val() != rt.Val() {
		return ext.BoolValue{Value: ext.Known(false)}
	}
	switch lt.Val().(type) {
	case ext.UndefinedType, ext.NullType:
		return ext.BoolValue{Value: ext.Known(true)}
	case ext.NumberType:
		lv := ext.AsPureNumber(left)
		rv := ext.AsPureNumber(right)
		if lv.Unknown() || rv.Unknown() {
			return ext.BoolValue{Value: ext.Unknown[bool]()}
		}
		return ext.BoolValue{Value: ext.Known(lv.Val() == rv.Val())}
	case ext.StringType:
		lv := ext.AsPureString(left)
		rv := ext.AsPureString(right)
		if lv.Unknown() || rv.Unknown() {
			return ext.BoolValue{Value: ext.Unknown[bool]()}
		}
		if strings.ContainsRune(lv.Val(), '\u000B') || strings.ContainsRune(rv.Val(), '\u000B') {
			return ext.BoolValue{Value: ext.Unknown[bool]()}
		}
		return ext.BoolValue{Value: ext.Known(lv.Val() == rv.Val())}
	case ext.BoolType:
		lv := ext.AsPureBool(left)
		rv := ext.AsPureBool(right)
		return lv.And(rv).Or(lv.Not().And(rv.Not()))
	}
	return ext.BoolValue{Value: ext.Unknown[bool]()}
}

func (s *simplifier) VisitAssignExpression(n *ast.AssignExpression) {
	old := s.isModifying
	s.isModifying = true
	n.Left.VisitWith(s)
	s.isModifying = old
	s.isModifying = false
	n.Right.VisitWith(s)
	s.isModifying = old
}

func (s *simplifier) VisitCallExpression(n *ast.CallExpression) {
	oldInCallee := s.inCallee
	s.inCallee = true
	mayInjectZero := !needZeroForThis(n.Callee)

	if seq, ok := n.Callee.Sequence(); ok {
		if len(seq.Sequence) == 1 {
			expr := seq.Sequence[0]
			expr.VisitWith(s)
			*n.Callee = expr
		} else if len(seq.Sequence) > 0 && directnessMaters(&seq.Sequence[len(seq.Sequence)-1]) {
			first := seq.Sequence[0]
			if !first.IsNumLit() && !first.IsIdent() {
				seq.Sequence = append([]ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0})}, seq.Sequence...)
			}
			seqExpr := ast.NewSequenceExpr(seq)
			seqExpr.VisitWith(s)
			*n.Callee = seqExpr
		}
	} else {
		n.Callee.VisitChildrenWith(s)
	}

	if mayInjectZero && needZeroForThis(n.Callee) {
		if seq, ok := n.Callee.Sequence(); ok {
			seq.Sequence = append([]ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0})}, seq.Sequence...)
		} else {
			callee := *n.Callee
			*n.Callee = ast.NewSequenceExpr(&ast.SequenceExpression{
				Sequence: []ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}), callee},
			})
		}
	}

	s.inCallee = false
	n.ArgumentList.VisitWith(s)
	s.inCallee = oldInCallee
}

func (s *simplifier) VisitExpression(n *ast.Expression) {
	if unaryExpr, ok := n.Unary(); ok && unaryExpr.Operator == ast.UnaryDelete {
		return
	}
	n.VisitChildrenWith(s)

	switch n.Kind() {
	case ast.ExprStrLit, ast.ExprBoolLit, ast.ExprNullLit, ast.ExprNumLit, ast.ExprRegExpLit, ast.ExprThis:
		return
	case ast.ExprSequence:
		if len(n.MustSequence().Sequence) == 0 {
			return
		}
	case ast.ExprUnary, ast.ExprBinary, ast.ExprLogical, ast.ExprMember, ast.ExprConditional, ast.ExprArrLit, ast.ExprObjLit, ast.ExprNew:
	default:
		return
	}

	switch n.Kind() {
	case ast.ExprUnary:
		s.optimizeUnaryExpression(n)
	case ast.ExprBinary:
		s.optimizeBinaryExpression(n)
	case ast.ExprLogical:
		s.optimizeLogicalExpression(n)
	case ast.ExprMember:
		s.optimizeMemberExpression(n)
	case ast.ExprConditional:
		cond := n.MustConditional()
		if v, pure := ext.CastToBool(cond.Test); v.Known() {
			s.changed = true
			var val *ast.Expression
			if v.Val() {
				val = cond.Consequent
			} else {
				val = cond.Alternate
			}
			if pure {
				if directnessMaters(val) {
					*n = ast.NewSequenceExpr(&ast.SequenceExpression{
						Sequence: []ast.Expression{ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}), *val},
					})
				} else {
					*n = *val
				}
			} else {
				*n = ast.NewSequenceExpr(&ast.SequenceExpression{
					Sequence: []ast.Expression{*cond.Test, *val},
				})
			}
		}
	case ast.ExprSequence:
		if seq := n.MustSequence(); len(seq.Sequence) == 1 {
			*n = seq.Sequence[0]
		}
	case ast.ExprArrLit:
		arr := n.MustArrLit()
		var exprs []ast.Expression
		for _, elem := range arr.Value {
			if spread, ok := elem.Spread(); ok {
				if arrLit, ok := spread.Expression.ArrLit(); ok {
					s.changed = true
					exprs = append(exprs, arrLit.Value...)
				} else {
					exprs = append(exprs, elem)
				}
			} else {
				exprs = append(exprs, elem)
			}
		}
		arr.Value = exprs
	case ast.ExprObjLit:
		obj := n.MustObjLit()
		if !slices.ContainsFunc(obj.Value, func(e ast.Property) bool { return e.IsSpread() }) {
			return
		}
		var props []ast.Property
		for _, prop := range obj.Value {
			if spread, ok := prop.Spread(); ok {
				if spreadObj, ok := spread.Expression.ObjLit(); ok {
					s.changed = true
					props = append(props, spreadObj.Value...)
				} else {
					props = append(props, prop)
				}
			} else {
				props = append(props, prop)
			}
		}
		obj.Value = props
	}
}

func (s *simplifier) VisitMemberExpression(n *ast.MemberExpression) {
	n.VisitChildrenWith(s)
	if compProp, ok := n.Property.Computed(); ok {
		if strLit, ok := compProp.Expr.StrLit(); ok && isIdentifier(strLit.Value) {
			s.changed = true
			*n.Property = ast.NewIdentMemProp(&ast.Identifier{Idx: n.Idx0(), Name: strLit.Value})
		}
	}
}

func (s *simplifier) VisitOptionalChain(*ast.OptionalChain) {}

func (s *simplifier) VisitVariableDeclarator(n *ast.VariableDeclarator) {
	if n.Initializer != nil {
		if seq, ok := n.Initializer.Sequence(); ok && len(seq.Sequence) == 0 {
			n = nil
		}
	}
	n.VisitChildrenWith(s)
}

func (s *simplifier) VisitSequenceExpression(n *ast.SequenceExpression) {
	if len(n.Sequence) == 0 {
		return
	}
	oldInCallee := s.inCallee
	length := len(n.Sequence)
	for i := range n.Sequence {
		if i == length-1 {
			s.inCallee = oldInCallee
		} else {
			s.inCallee = false
		}
		n.Sequence[i].VisitWith(s)
	}
	s.inCallee = oldInCallee

	length = len(n.Sequence)
	last := n.Sequence[length-1]
	var exprs []ast.Expression
	for _, expr := range n.Sequence[:length-1] {
		if numLit, ok := expr.NumLit(); ok && s.inCallee && numLit.Value == 0.0 {
			if len(exprs) == 0 {
				exprs = append(exprs, ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}))
			}
			continue
		}
		if s.inCallee && !ext.MayHaveSideEffects(&expr) {
			switch expr.Kind() {
			case ast.ExprStrLit, ast.ExprBoolLit, ast.ExprNullLit, ast.ExprNumLit, ast.ExprRegExpLit, ast.ExprIdent:
				if len(exprs) == 0 {
					s.changed = true
					exprs = append(exprs, ast.NewNumLitExpr(&ast.NumberLiteral{Value: 0.0}))
				}
				continue
			}
		}
		switch expr.Kind() {
		case ast.ExprStrLit, ast.ExprBoolLit, ast.ExprNullLit, ast.ExprNumLit, ast.ExprRegExpLit, ast.ExprIdent:
			continue
		}
		if arrLit, ok := expr.ArrLit(); ok {
			isSimple := !slices.ContainsFunc(arrLit.Value, func(e ast.Expression) bool { return e.IsSpread() })
			if isSimple {
				exprs = append(exprs, arrLit.Value...)
			} else {
				exprs = append(exprs, ast.NewArrLitExpr(&ast.ArrayLiteral{Value: arrLit.Value}))
			}
			continue
		}
		exprs = append(exprs, expr)
	}
	exprs = append(exprs, last)
	s.changed = s.changed || len(exprs) != len(n.Sequence)
	n.Sequence = exprs
}

func (s *simplifier) VisitStatement(n *ast.Statement) {
	oldIsModifying := s.isModifying
	s.isModifying = false
	oldIsArgOfUpdate := s.isArgOfUpdate
	s.isArgOfUpdate = false
	n.VisitChildrenWith(s)
	s.isArgOfUpdate = oldIsArgOfUpdate
	s.isModifying = oldIsModifying
}

func (s *simplifier) VisitUpdateExpression(n *ast.UpdateExpression) {
	old := s.isModifying
	s.isModifying = true
	n.Operand.VisitWith(s)
	s.isModifying = old
}

func (s *simplifier) VisitForInStatement(n *ast.ForInStatement) {
	old := s.isModifying
	s.isModifying = true
	n.VisitChildrenWith(s)
	s.isModifying = old
}

func (s *simplifier) VisitForOfStatement(n *ast.ForOfStatement) {
	old := s.isModifying
	s.isModifying = true
	n.VisitChildrenWith(s)
	s.isModifying = old
}

func (s *simplifier) VisitTemplateLiteral(n *ast.TemplateLiteral) {
	if n.Tag != nil {
		old := s.inCallee
		s.inCallee = true
		n.Tag.VisitWith(s)
		s.inCallee = false
		n.Expressions.VisitWith(s)
		s.inCallee = old
	}
}

func (s *simplifier) VisitWithStatement(n *ast.WithStatement) {
	n.Object.VisitWith(s)
}

// Simplify simplifies the AST by optimizing expressions.
func Simplify(p ast.VisitableNode, resolve bool) {
	if resolve {
		resolver.Resolve(p)
	}

	visitor := &simplifier{}
	visitor.V = visitor
	p.VisitWith(visitor)
}
