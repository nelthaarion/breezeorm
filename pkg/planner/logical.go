// Package planner turns a compiled query.Builder AST into a LogicalPlan
// (dialect-agnostic relational algebra tree), and then, after optimization,
// into a PhysicalPlan (dialect-aware, ready for SQL generation).
package planner

import (
	"github.com/nelthaarion/breezorm/pkg/dialect"
	"github.com/nelthaarion/breezorm/pkg/query"
)

// NodeKind identifies the relational-algebra operator a LogicalNode represents.
type NodeKind uint8

const (
	NodeScan NodeKind = iota
	NodeFilter
	NodeProject
	NodeJoin
	NodeAggregate
	NodeSort
	NodeLimit
	NodeDistinct
	NodeUnion
	NodeCTE
	NodeInsert
	NodeUpdate
	NodeDelete
	NodeUpsert
)

// LogicalNode is one node of the logical plan tree. It carries only
// relational-algebra semantics — no SQL syntax, no dialect concerns.
type LogicalNode struct {
	Kind        NodeKind
	Table       string
	Alias       string
	Input       *LogicalNode   // single-child operators (Filter, Project, Sort, Limit, Distinct)
	Inputs      []*LogicalNode // multi-child operators (Join, Union)
	Predicate   query.Expr
	Projections []query.SelectExpr
	JoinKind    query.JoinKind
	JoinOn      query.Expr
	GroupBy     []string
	Having      query.Expr
	OrderBy     []query.OrderTerm
	Limit       *int64
	Offset      *int64
	CTEs        []query.CTE
	Assignments []query.Assignment
	Lock        dialect.LockMode
}

// LogicalPlan is the root of a compiled, dialect-agnostic query plan.
type LogicalPlan struct {
	Root *LogicalNode
}

// Build constructs a LogicalPlan from a query.Builder's exposed AST.
// This is a straightforward, mostly mechanical lowering; the interesting
// transformations happen in pkg/optimizer.
func Build[T any](b query.Builder[T]) *LogicalPlan {
	var root *LogicalNode

	switch b.StmtKind() {
	case query.KindInsert:
		root = &LogicalNode{Kind: NodeInsert, Table: b.Table(), Assignments: b.Assignments()}
	case query.KindUpdate:
		root = &LogicalNode{Kind: NodeUpdate, Table: b.Table(), Assignments: b.Assignments(), Predicate: b.WhereExpr()}
	case query.KindDelete:
		root = &LogicalNode{Kind: NodeDelete, Table: b.Table(), Predicate: b.WhereExpr()}
	case query.KindUpsert:
		root = &LogicalNode{Kind: NodeUpsert, Table: b.Table(), Assignments: b.Assignments()}
	default:
		root = &LogicalNode{Kind: NodeScan, Table: b.Table()}

		for _, j := range b.Joins() {
			root = &LogicalNode{
				Kind:     NodeJoin,
				JoinKind: j.Kind,
				JoinOn:   j.Condition,
				Table:    j.Table,
				Alias:    j.Alias,
				Inputs:   []*LogicalNode{root},
			}
		}

		if w := b.WhereExpr(); w != nil {
			root = &LogicalNode{Kind: NodeFilter, Predicate: w, Input: root}
		}

		if len(b.GroupByCols()) > 0 || b.HavingExpr() != nil {
			root = &LogicalNode{Kind: NodeAggregate, GroupBy: b.GroupByCols(), Having: b.HavingExpr(), Input: root}
		}

		if sel := b.Selects(); len(sel) > 0 {
			root = &LogicalNode{Kind: NodeProject, Projections: sel, Input: root}
		}

		if b.Distincted() {
			root = &LogicalNode{Kind: NodeDistinct, Input: root}
		}

		if len(b.OrderByTerms()) > 0 {
			root = &LogicalNode{Kind: NodeSort, OrderBy: b.OrderByTerms(), Input: root}
		}

		if b.LimitVal() != nil || b.OffsetVal() != nil {
			root = &LogicalNode{Kind: NodeLimit, Limit: b.LimitVal(), Offset: b.OffsetVal(), Input: root}
		}

		if len(b.CTEs()) > 0 {
			root = &LogicalNode{Kind: NodeCTE, CTEs: b.CTEs(), Input: root}
		}

		root.Lock = b.LockMode()
	}

	return &LogicalPlan{Root: root}
}
