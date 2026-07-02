package simplifier

import (
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/t14raptor/go-fast/ast"
	"github.com/t14raptor/go-fast/ast/ext"
)

func isNonObj(n *ast.Expression) bool {
	switch n.Kind() {
	case ast.ExprStringLit, ast.ExprNumberLit, ast.ExprNullLit, ast.ExprBoolLit:
		return true
	case ast.ExprIdentifier:
		name := n.MustIdentifier().Name
		if name == "undefined" || name == "Infinity" || name == "NaN" {
			return true
		}
	case ast.ExprUnary:
		u := n.MustUnary()
		if u.Operator == ast.UnaryLogicalNot || u.Operator == ast.UnaryNegation || u.Operator == ast.UnaryVoid {
			return isNonObj(u.Operand)
		}
	}
	return false
}

func isObj(n *ast.Expression) bool {
	switch n.Kind() {
	case ast.ExprArrayLit, ast.ExprObjectLit, ast.ExprFuncLit, ast.ExprNew:
		return true
	default:
		return false
	}
}

func directnessMaters(n *ast.Expression) bool {
	switch n.Kind() {
	case ast.ExprIdentifier:
		return n.MustIdentifier().Name == "eval"
	case ast.ExprMember:
		return true
	}
	return false
}

func makeBoolExpr(value bool, orig ast.Expressions) ast.Expression {
	return ext.PreserveEffects(ast.NewBoolLitExpr(&ast.BooleanLiteral{Value: value}), orig)
}

func nthChar(s string, idx int) (string, bool) {
	for _, c := range s {
		if len(utf16.Encode([]rune{c})) > 1 {
			return "", false
		}
	}

	if !strings.Contains(s, "\\ud") && !strings.Contains(s, "\\uD") {
		if idx < len([]rune(s)) {
			return string([]rune(s)[idx]), true
		}
		return "", false
	}

	iter := []rune(s)
	for i := 0; i < len(iter); i++ {
		c := iter[i]
		if c == '\\' && i+1 < len(iter) && iter[i+1] == 'u' {
			if idx == 0 {
				if i+5 < len(iter) {
					return string(iter[i : i+6]), true
				}
				return "", false
			}
			i += 5
		} else {
			if idx == 0 {
				return string(c), true
			}
		}
		idx--
	}

	return "", false
}

func needZeroForThis(e *ast.Expression) bool {
	return directnessMaters(e) || e.IsSequence()
}

func getKeyValue(props []ast.Property, key string) *ast.Expression {
	// It's impossible to know the value for certain if a spread property exists.
	if slices.ContainsFunc(props, func(p ast.Property) bool {
		return p.IsSpread()
	}) {
		return nil
	}

	for _, prop := range slices.Backward(props) {
		switch prop.Kind() {
		case ast.PropShort:
			short := prop.MustShort()
			if short.Name.Name == key {
				e := ast.NewIdentifierExpr(short.Name)
				return &e
			}
		case ast.PropKeyValue:
			keyed := prop.MustKeyValue()
			if key != "__proto__" && propNameEq(keyed.Key, "__proto__") {
				// If __proto__ is defined, we need to check the contents of it,
				// as well as any nested __proto__ objects
				if obj, ok := keyed.Value.ObjectLit(); ok {
					if v := getKeyValue(obj.Value, key); v != nil {
						return v
					}
				}
				return nil
			} else if propNameEq(keyed.Key, key) {
				return keyed.Value
			}
		}
	}

	return nil
}

func ptrExpr(e ast.Expression) *ast.Expression {
	return &e
}

func propNameEq(key *ast.PropertyName, name string) bool {
	switch key.Kind() {
	case ast.PropNameStringLit:
		return key.MustStringLit().Value == name
	case ast.PropNameNumberLit:
		return strconv.FormatFloat(key.MustNumberLit().Value, 'f', -1, 64) == name
	}
	return false
}

// toInt32 implements JavaScript's ToInt32 abstract operation.
func toInt32(v float64) int32 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v == 0 {
		return 0
	}
	n := math.Copysign(math.Floor(math.Abs(v)), v)
	n = math.Mod(n, 4294967296) // 2^32
	if n < 0 {
		n += 4294967296
	}
	if n >= 2147483648 { // 2^31
		return int32(n - 4294967296)
	}
	return int32(n)
}

// toUint32 implements JavaScript's ToUint32 abstract operation.
func toUint32(v float64) uint32 {
	if math.IsNaN(v) || math.IsInf(v, 0) || v == 0 {
		return 0
	}
	n := math.Copysign(math.Floor(math.Abs(v)), v)
	n = math.Mod(n, 4294967296)
	if n < 0 {
		n += 4294967296
	}
	return uint32(n)
}
