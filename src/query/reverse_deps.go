package query

import (
	"container/list"
	"fmt"
	"sort"

	"github.com/thought-machine/please/src/core"
)

// ReverseDeps finds all transitive targets that depend on the set of input labels.
func ReverseDeps(state *core.BuildState, labels []core.BuildLabel, level int, hidden bool) {
	targets := FindRevdeps(state, labels, hidden, level)
	ls := make(core.BuildLabels, 0, len(targets))

	for target := range targets {
		if state.ShouldInclude(target) {
			ls = append(ls, target.Label)
		}
	}
	sort.Sort(ls)

	for _, l := range ls {
		fmt.Println(l.String())
	}
}

// node represents a node in the build graph and the depth we visited it at.
type node struct {
	target *core.BuildTarget
	depth  int
}

// openSet represents the queue of nodes we need to process in the graph. There are no duplicates in this set and the
// queue will be ordered low to high by depth i.e. in the order they must be processed
//
// NB: We don't need to explicitly order this. Paths either cost 1 or 0, but all 0 cost paths are equivalent e.g. paths
// :lib1 -> :_lib1#foo -> :lib2, lib1 -> :_lib1#foo -> :_lib1#bar -> :lib2, and :lib1 -> lib2 all have a cost of 1 and
// will result in :lib1 and :lib2 as outputs. It doesn't matter which is explored to generate the output.
type openSet struct {
	items *list.List

	// done contains a map of targets we've already processed.
	done map[core.BuildLabel]struct{}
}

// Push implements pushing a node onto the queue of nodes to process, deduplicating nodes we've seen before.
func (os *openSet) Push(n *node) {
	if _, present := os.done[n.target.Label]; !present {
		os.done[n.target.Label] = struct{}{}
		os.items.PushBack(n)
	}
}

// Pop fetches the next node off the queue for us to process
func (os *openSet) Pop() *node {
	next := os.items.Front()
	if next == nil {
		return nil
	}
	os.items.Remove(next)

	return next.Value.(*node)
}

type revdeps struct {
	// subincludes is a map of build labels to the packages that subinclude them
	subincludes map[core.BuildLabel][]*core.Package

	// os is the open set of targets to process
	os *openSet

	// hidden is whether to count hidden targets towards the depth budget
	hidden bool

	// maxDepth is the depth budget for the search. -1 means unlimited.
	maxDepth int
}

// newRevdeps creates a new reverse dependency searcher. revdeps is non-reusable.
func newRevdeps(graph *core.BuildGraph, hidden bool, maxDepth int) *revdeps {
	// Initialise a map of labels to the packages that subinclude them upfront so we can include those targets as
	// dependencies efficiently later
	subincludes := make(map[core.BuildLabel][]*core.Package)
	for _, pkg := range graph.PackageMap() {
		for _, inc := range pkg.Subincludes {
			subincludes[inc] = append(subincludes[inc], pkg)
		}
	}

	return &revdeps{
		subincludes: subincludes,
		os: &openSet{
			items: list.New(),
			done:  map[core.BuildLabel]struct{}{},
		},
		hidden:   hidden,
		maxDepth: maxDepth,
	}
}

// FindRevdeps will return a set of build targets that are reverse dependencies of the provided labels.
func FindRevdeps(state *core.BuildState, targets core.BuildLabels, hidden bool, depth int) map[*core.BuildTarget]struct{} {
	r := newRevdeps(state.Graph, hidden, depth)
	// Initialise the open set with the original targets
	for _, t := range targets {
		r.os.Push(&node{
			target: state.Graph.TargetOrDie(t),
			depth:  0,
		})
	}
	return r.findRevdeps(state)
}

func (r *revdeps) findRevdeps(state *core.BuildState) map[*core.BuildTarget]struct{} {
	// 1000 is chosen pretty much arbitrarily here
	ret := make(map[*core.BuildTarget]struct{}, 1000)
	for next := r.os.Pop(); next != nil; next = r.os.Pop() {
		ts := state.Graph.ReverseDependencies(next.target)

		for _, p := range r.subincludes[next.target.Label] {
			ts = append(ts, p.AllTargets()...)
		}

		for _, t := range ts {
			depth := next.depth
			parent := t.Parent(state.Graph)

			// The label shouldn't count towards the depth if it's a child of the last label
			if r.hidden || parent == nil || parent != next.target {
				depth++
			}

			// We can skip adding to the open set if the depth of the next non-child label pushes us over the budget
			// but we must make sure to add child labels at the current depth.
			if (next.depth+1) <= r.maxDepth || r.maxDepth == -1 {
				if r.hidden || !t.Label.IsHidden() {
					ret[t] = struct{}{}
				}

				r.os.Push(&node{
					target: t,
					depth:  depth,
				})
			}
		}
	}
	return ret
}
