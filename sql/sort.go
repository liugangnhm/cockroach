// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// Author: Peter Mattis (peter@cockroachlabs.com)

package sql

import (
	"fmt"
	"sort"
	"strings"

	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/sql/parser"
	"github.com/cockroachdb/cockroach/util/encoding"
	"github.com/cockroachdb/cockroach/util/log"
)

// orderBy constructs a sortNode based on the ORDER BY clause. Construction of
// the sortNode might adjust the number of render targets in the selectNode if
// any ordering expressions are specified.
func (p *planner) orderBy(n *parser.Select, s *selectNode) (*sortNode, *roachpb.Error) {
	if n.OrderBy == nil {
		return nil, nil
	}

	// We grab a copy of columns here because we might add new render targets
	// below. This is the set of columns requested by the query.
	columns := s.Columns()
	var ordering columnOrdering

	for _, o := range n.OrderBy {
		index := -1

		// Normalize the expression which has the side-effect of evaluating
		// constant expressions and unwrapping expressions like "((a))" to "a".
		expr, err := p.parser.NormalizeExpr(p.evalCtx, o.Expr)
		if err != nil {
			return nil, roachpb.NewError(err)
		}

		if qname, ok := expr.(*parser.QualifiedName); ok {
			if len(qname.Indirect) == 0 {
				// Look for an output column that matches the qualified name. This
				// handles cases like:
				//
				//   SELECT a AS b FROM t ORDER BY b
				target := string(qname.Base)
				for j, col := range columns {
					if equalName(target, col.Name) {
						index = j
						break
					}
				}
			}

			if index == -1 {
				// No output column matched the qualified name, so look for an existing
				// render target that matches the column name. This handles cases like:
				//
				//   SELECT a AS b FROM t ORDER BY a
				if err := qname.NormalizeColumnName(); err != nil {
					return nil, roachpb.NewError(err)
				}
				if qname.Table() == "" || equalName(s.table.alias, qname.Table()) {
					for j, r := range s.render {
						if qval, ok := r.(*qvalue); ok {
							if equalName(qval.colRef.get().Name, qname.Column()) {
								index = j
								break
							}
						}
					}
				}
			}
		}

		if index == -1 {
			// The order by expression matched neither an output column nor an
			// existing render target.
			if col, err := s.colIndex(expr); err != nil {
				return nil, roachpb.NewError(err)
			} else if col >= 0 {
				index = col
			} else {
				// Add a new render expression to use for ordering. This handles cases
				// were the expression is either not a qualified name or is a qualified
				// name that is otherwise not referenced by the query:
				//
				//   SELECT a FROM t ORDER by b
				//   SELECT a, b FROM t ORDER by a+b
				if err := s.addRender(parser.SelectExpr{Expr: expr}); err != nil {
					return nil, err
				}
				index = len(s.columns) - 1
			}
		}
		direction := encoding.Ascending
		if o.Direction == parser.Descending {
			direction = encoding.Descending
		}
		ordering = append(ordering, columnOrderInfo{index, direction})
	}

	return &sortNode{columns: columns, ordering: ordering}, nil
}

type sortNode struct {
	plan     planNode
	columns  []ResultColumn
	ordering columnOrdering
	needSort bool
	pErr     *roachpb.Error
}

func (n *sortNode) Columns() []ResultColumn {
	return n.columns
}

func (n *sortNode) Ordering() orderingInfo {
	if n == nil {
		return orderingInfo{}
	}
	return orderingInfo{exactMatchCols: nil, ordering: n.ordering}
}

func (n *sortNode) Values() parser.DTuple {
	// If an ordering expression was used the number of columns in each row might
	// differ from the number of columns requested, so trim the result.
	v := n.plan.Values()
	return v[:len(n.columns)]
}

func (*sortNode) DebugValues() debugValues {
	// TODO(radu)
	panic("debug mode not implemented in sortNode")
}

func (n *sortNode) Next() bool {
	if n.needSort {
		n.needSort = false
		if !n.initValues() {
			return false
		}
	}
	return n.plan.Next()
}

func (n *sortNode) PErr() *roachpb.Error {
	return n.pErr
}

func (n *sortNode) ExplainPlan() (name, description string, children []planNode) {
	if n.needSort {
		name = "sort"
	} else {
		name = "nosort"
	}

	columns := n.plan.Columns()
	strs := make([]string, len(n.ordering))
	for i, o := range n.ordering {
		prefix := '+'
		if o.direction == encoding.Descending {
			prefix = '-'
		}
		strs[i] = fmt.Sprintf("%c%s", prefix, columns[o.colIdx].Name)
	}
	description = strings.Join(strs, ",")

	return name, description, []planNode{n.plan}
}

func (n *sortNode) SetLimitHint(numRows int64) {
	// The limit is only useful to the wrapped node if we don't need to sort.
	if !n.needSort {
		n.plan.SetLimitHint(numRows)
	}
}

// wrap the supplied planNode with the sortNode if sorting is required.
func (n *sortNode) wrap(plan planNode) planNode {
	if n != nil {
		// Check to see if the requested ordering is compatible with the existing
		// ordering.
		existingOrdering := plan.Ordering()
		if log.V(2) {
			log.Infof("Sort: existing=%d desired=%d", existingOrdering, n.ordering)
		}
		match := computeOrderingMatch(n.ordering, existingOrdering, false)
		if match < len(n.ordering) {
			n.plan = plan
			n.needSort = true
			return n
		}

		if len(n.columns) < len(plan.Columns()) {
			// No sorting required, but we have to strip off the extra render
			// expressions we added.
			n.plan = plan
			return n
		}
	}

	if log.V(2) {
		log.Infof("Sort: no sorting required")
	}
	return plan
}

func (n *sortNode) initValues() bool {
	// TODO(pmattis): If the result set is large, we might need to perform the
	// sort on disk.
	var v *valuesNode
	if x, ok := n.plan.(*valuesNode); ok {
		v = x
		v.ordering = n.ordering
	} else {
		v = &valuesNode{ordering: n.ordering}
		// TODO(andrei): If we're scanning an index with a prefix matching an
		// ordering prefix, we should only accumulate values for equal fields
		// in this prefix, then sort the accumulated chunk and output.
		for n.plan.Next() {
			values := n.plan.Values()
			valuesCopy := make(parser.DTuple, len(values))
			copy(valuesCopy, values)
			v.rows = append(v.rows, valuesCopy)
		}
		n.pErr = n.plan.PErr()
		if n.pErr != nil {
			return false
		}
	}
	sort.Sort(v)
	n.plan = v
	return true
}
