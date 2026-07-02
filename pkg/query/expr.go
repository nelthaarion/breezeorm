// Package query implements the immutable, generic fluent query builder AST.
// Builder values never mutate in place — every method returns a new Builder,
// making instances safe to share, branch from, and cache.
package query

// Op is a comparison / logical operator in a predicate expression.
type Op string

const (
	OpEq        Op = "="
	OpNeq       Op = "<>"
	OpLt        Op = "<"
	OpLte       Op = "<="
	OpGt        Op = ">"
	OpGte       Op = ">="
	OpLike      Op = "LIKE"
	OpILike     Op = "ILIKE"
	OpIn        Op = "IN"
	OpNotIn     Op = "NOT IN"
	OpIsNull    Op = "IS NULL"
	OpIsNotNull Op = "IS NOT NULL"
	OpBetween   Op = "BETWEEN"
	OpAnd       Op = "AND"
	OpOr        Op = "OR"
	OpNot       Op = "NOT"
	OpRaw       Op = "RAW"
)

// Expr is a node in the predicate AST. It is a closed sum type implemented
// by the types below (Predicate, LogicalExpr, RawExpr, SubqueryExpr).
type Expr interface {
	isExpr()
}

// Predicate is a leaf comparison, e.g. `age >= 18`.
type Predicate struct {
	Column string
	Op     Op
	Value  any // may be a scalar, []any (IN), [2]any (BETWEEN), or *Builder (subquery)
}

func (Predicate) isExpr() {}

// LogicalExpr combines child expressions with AND/OR/NOT.
type LogicalExpr struct {
	Op       Op
	Children []Expr
}

func (LogicalExpr) isExpr() {}

// RawExpr is an escape hatch for raw SQL fragments with named/positional args.
type RawExpr struct {
	SQL  string
	Args []any
}

func (RawExpr) isExpr() {}

// SubqueryExpr wraps a nested Builder used as an expression (EXISTS, IN, scalar subquery).
type SubqueryExpr struct {
	Kind    SubqueryKind
	Builder any // *Builder[U] for some U; held as any to avoid infecting Expr with type params
}

func (SubqueryExpr) isExpr() {}

type SubqueryKind uint8

const (
	SubqueryExists SubqueryKind = iota
	SubqueryNotExists
	SubqueryIn
	SubqueryScalar
)

// And combines expressions with AND, flattening nested AND groups.
func And(exprs ...Expr) Expr {
	if len(exprs) == 1 {
		return exprs[0]
	}
	return LogicalExpr{Op: OpAnd, Children: exprs}
}

// Or combines expressions with OR.
func Or(exprs ...Expr) Expr {
	if len(exprs) == 1 {
		return exprs[0]
	}
	return LogicalExpr{Op: OpOr, Children: exprs}
}

// Not negates an expression.
func Not(e Expr) Expr {
	return LogicalExpr{Op: OpNot, Children: []Expr{e}}
}

// Raw builds a RawExpr with named (:name) or positional (?) parameters.
func Raw(sql string, args ...any) Expr {
	return RawExpr{SQL: sql, Args: args}
}
