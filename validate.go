package taskplanner

import (
	"fmt"
	"sort"
	"strings"
)

func invalidf(format string, args ...any) error {
	return &PlanError{Kind: FailureInvalidDomain, Message: fmt.Sprintf(format, args...)}
}

func checkLiterals(owner string, lits []Literal, params map[string]struct{}) error {
	for _, lit := range lits {
		if lit.Predicate == "" {
			return invalidf("%s: literal has empty predicate", owner)
		}
		for _, a := range lit.Args {
			if strings.HasPrefix(a, "?") {
				if _, ok := params[a]; !ok {
					return invalidf("%s: literal %s references unbound variable %s", owner, atomKey(lit), a)
				}
			}
		}
	}
	return nil
}

func checkParams(owner string, params []string) (map[string]struct{}, error) {
	set := make(map[string]struct{}, len(params))
	for _, p := range params {
		if p == "" {
			return nil, invalidf("%s: has empty parameter name", owner)
		}
		if !strings.HasPrefix(p, "?") {
			return nil, invalidf("%s: parameter %s must start with '?'", owner, p)
		}
		if _, dup := set[p]; dup {
			return nil, invalidf("%s: duplicate parameter %s", owner, p)
		}
		set[p] = struct{}{}
	}
	return set, nil
}

func validateDomain(d *Domain) error {
	if d == nil {
		return invalidf("domain is nil")
	}

	actionNames := make([]string, 0, len(d.Actions))
	for name, a := range d.Actions {
		if a == nil {
			return invalidf("action %q declaration is nil", name)
		}
		if a.Name == "" {
			a.Name = name
		}
		if a.Name != name {
			return invalidf("action registered as %q but named %q", name, a.Name)
		}
		actionNames = append(actionNames, name)
	}
	sort.Strings(actionNames)

	compoundNames := make([]string, 0, len(d.Compounds))
	for name, c := range d.Compounds {
		if c == nil {
			return invalidf("compound task %q declaration is nil", name)
		}
		if c.Name == "" {
			c.Name = name
		}
		if c.Name != name {
			return invalidf("compound task registered as %q but named %q", name, c.Name)
		}
		if _, isAction := d.Actions[name]; isAction {
			return invalidf("%q is declared both as action and compound task", name)
		}
		compoundNames = append(compoundNames, name)
	}
	sort.Strings(compoundNames)

	for _, name := range actionNames {
		a := d.Actions[name]
		params, err := checkParams("action "+name, a.Params)
		if err != nil {
			return err
		}
		if err := checkLiterals("action "+name, a.Pre, params); err != nil {
			return err
		}
		if err := checkLiterals("action "+name, a.Add, params); err != nil {
			return err
		}
		if err := checkLiterals("action "+name, a.Del, params); err != nil {
			return err
		}
	}

	for _, name := range compoundNames {
		c := d.Compounds[name]
		params, err := checkParams("compound task "+name, c.Params)
		if err != nil {
			return err
		}
		methodNames := make(map[string]struct{})
		for mi, m := range c.Methods {
			owner := fmt.Sprintf("compound task %s method #%d (%q)", name, mi, m.Name)
			if m.Name != "" {
				if _, dup := methodNames[m.Name]; dup {
					return invalidf("compound task %s has duplicate method name %q", name, m.Name)
				}
				methodNames[m.Name] = struct{}{}
			}
			if err := checkLiterals(owner, m.Pre, params); err != nil {
				return err
			}
			for gi, group := range m.Groups {
				for ti, t := range group {
					tOwner := fmt.Sprintf("%s group %d task %d", owner, gi, ti)
					if t.Name == "" {
						return invalidf("%s: empty task name", tOwner)
					}
					ac, isAction := d.Actions[t.Name]
					cp, isCompound := d.Compounds[t.Name]
					switch {
					case isAction && isCompound:
						return invalidf("%s: %q is ambiguous", tOwner, t.Name)
					case isAction:
						if len(t.Args) != len(ac.Params) {
							return invalidf("%s: action %s expects %d args, got %d", tOwner, t.Name, len(ac.Params), len(t.Args))
						}
					case isCompound:
						if len(t.Args) != len(cp.Params) {
							return invalidf("%s: compound task %s expects %d args, got %d", tOwner, t.Name, len(cp.Params), len(t.Args))
						}
					default:
						return invalidf("%s: references unknown task %q", tOwner, t.Name)
					}
					for _, a := range t.Args {
						if strings.HasPrefix(a, "?") {
							if _, ok := params[a]; !ok {
								return invalidf("%s: argument %s is not bound by enclosing task", tOwner, a)
							}
						}
					}
				}
			}
		}
	}

	return detectDecompositionCycle(d, compoundNames)
}

// detectDecompositionCycle 检测复合任务静态分解依赖图中的环（含自环）。
func detectDecompositionCycle(d *Domain, roots []string) error {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := make(map[string]int, len(d.Compounds))
	var stack []string

	var visit func(name string) error
	visit = func(name string) error {
		color[name] = gray
		stack = append(stack, name)

		c := d.Compounds[name]
		edges := make(map[string]struct{})
		for _, m := range c.Methods {
			for _, group := range m.Groups {
				for _, t := range group {
					if _, ok := d.Compounds[t.Name]; ok {
						edges[t.Name] = struct{}{}
					}
				}
			}
		}
		edgeNames := make([]string, 0, len(edges))
		for e := range edges {
			edgeNames = append(edgeNames, e)
		}
		sort.Strings(edgeNames)

		for _, e := range edgeNames {
			switch color[e] {
			case white:
				if err := visit(e); err != nil {
					return err
				}
			case gray:
				start := 0
				for i, n := range stack {
					if n == e {
						start = i
						break
					}
				}
				cycle := append(append([]string{}, stack[start:]...), e)
				return invalidf("recursive decomposition cycle detected: %s", strings.Join(cycle, " -> "))
			}
		}

		stack = stack[:len(stack)-1]
		color[name] = black
		return nil
	}

	for _, name := range roots {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}
