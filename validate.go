package taskplan

import (
	"sort"
	"strings"
)

// Validate checks the domain for structural problems: references to
// undeclared tasks, name collisions between actions and compound tasks,
// and recursive decomposition cycles. It returns a *Failure with kind
// FailureInvalidDomain when the domain is malformed.
//
// Recursion note: a compound task whose methods can (transitively)
// decompose back into itself forms a decomposition cycle. Such cycles
// are rejected here so the planner can never recurse forever.
func (d *Domain) Validate() error {
	if d == nil {
		return invalidDomainf("domain is nil")
	}
	for key, a := range d.Actions {
		if a == nil {
			return invalidDomainf("action %q is nil", key)
		}
		if a.Name == "" {
			return invalidDomainf("action with empty name declared under key %q", key)
		}
		if key != a.Name {
			return invalidDomainf("action registered as %q but named %q", key, a.Name)
		}
		if _, clash := d.Methods[a.Name]; clash {
			return invalidDomainf("name %q is declared both as action and as compound task", a.Name)
		}
	}
	for task, methods := range d.Methods {
		if task == "" {
			return invalidDomainf("compound task with empty name")
		}
		if len(methods) == 0 {
			return invalidDomainf("compound task %q has no decomposition methods", task)
		}
		for _, m := range methods {
			if m == nil {
				return invalidDomainf("compound task %q has a nil method", task)
			}
			if m.Name == "" {
				return invalidDomainf("compound task %q has a method with empty name", task)
			}
			for _, st := range m.Subtasks {
				if err := d.checkTaskRef(task, st); err != nil {
					return err
				}
			}
		}
	}
	return d.checkCycles()
}

// checkTaskRef verifies that a subtask reference points at a declared
// action or compound task.
func (d *Domain) checkTaskRef(parent string, ref TaskRef) error {
	switch ref.Kind {
	case PrimitiveTask:
		if _, ok := d.Actions[ref.Name]; !ok {
			return invalidDomainf("method of %q references unknown action %q", parent, ref.Name)
		}
	case CompoundTask:
		if _, ok := d.Methods[ref.Name]; !ok {
			return invalidDomainf("method of %q references unknown compound task %q", parent, ref.Name)
		}
	default:
		return invalidDomainf("method of %q references task %q with invalid kind %d", parent, ref.Name, ref.Kind)
	}
	return nil
}

// checkCycles detects cycles in the compound-task decomposition graph
// using an iterative three-color DFS. The traversal order is
// deterministic so the reported cycle is stable across runs.
func (d *Domain) checkCycles() error {
	const (
		white = iota // unvisited
		gray         // on the current DFS path
		black        // fully explored
	)
	color := make(map[string]int, len(d.Methods))
	tasks := make([]string, 0, len(d.Methods))
	for task := range d.Methods {
		tasks = append(tasks, task)
	}
	sort.Strings(tasks)

	type frame struct {
		task    string
		nextDep int
	}
	for _, start := range tasks {
		if color[start] != white {
			continue
		}
		color[start] = gray
		stack := []frame{{task: start}}
		path := []string{start}
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			deps := d.sortedCompoundDeps(top.task)
			if top.nextDep >= len(deps) {
				color[top.task] = black
				stack = stack[:len(stack)-1]
				path = path[:len(path)-1]
				continue
			}
			dep := deps[top.nextDep]
			top.nextDep++
			switch color[dep] {
			case white:
				color[dep] = gray
				stack = append(stack, frame{task: dep})
				path = append(path, dep)
			case gray:
				cycle := append(append([]string{}, path[indexOf(path, dep):]...), dep)
				return invalidDomainf("recursive decomposition cycle detected: %s", strings.Join(cycle, " -> "))
			}
		}
	}
	return nil
}

// sortedCompoundDeps returns the compound tasks directly referenced by
// the methods of task, deduplicated and sorted for determinism.
func (d *Domain) sortedCompoundDeps(task string) []string {
	seen := make(map[string]struct{})
	for _, m := range d.Methods[task] {
		for _, st := range m.Subtasks {
			if st.Kind == CompoundTask {
				seen[st.Name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func indexOf(ss []string, target string) int {
	for i, s := range ss {
		if s == target {
			return i
		}
	}
	return -1
}
