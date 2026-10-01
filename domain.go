package taskplanner

import (
	"sort"
	"strings"
)

// Literal 是一个一阶谓词字面量，Negated 为 true 时表示否定。
type Literal struct {
	Predicate string
	Args      []string
	Negated   bool
}

// State 是一组完全基例化的正事实（封闭世界假设）。
type State struct {
	atoms map[string]struct{}
}

func NewState(atoms ...Literal) *State {
	s := &State{atoms: make(map[string]struct{}, len(atoms))}
	for _, lit := range atoms {
		if !lit.Negated {
			s.atoms[atomKey(lit)] = struct{}{}
		}
	}
	return s
}

func (s *State) Clone() *State {
	if s == nil {
		return NewState()
	}
	c := &State{atoms: make(map[string]struct{}, len(s.atoms))}
	for k := range s.atoms {
		c.atoms[k] = struct{}{}
	}
	return c
}

func (s *State) Has(lit Literal) bool {
	_, ok := s.atoms[atomKey(lit)]
	if lit.Negated {
		return !ok
	}
	return ok
}

func (s *State) Apply(add, del []Literal) {
	for _, lit := range del {
		if lit.Negated {
			continue
		}
		delete(s.atoms, atomKey(lit))
	}
	for _, lit := range add {
		if lit.Negated {
			continue
		}
		s.atoms[atomKey(lit)] = struct{}{}
	}
}

func (s *State) AtomKeys() []string {
	keys := make([]string, 0, len(s.atoms))
	for k := range s.atoms {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Action 是基本动作（操作符）声明。
type Action struct {
	Name   string
	Params []string
	Pre    []Literal
	Add    []Literal
	Del    []Literal
}

// TaskExpr 是对任务（复合任务或动作）的一次引用，
// Args 中以 "?" 开头的元素引用父复合任务的形参，其余为常量。
type TaskExpr struct {
	Name string
	Args []string
}

// Method 是复合任务的一种分解方式：前置条件满足时，
// 将复合任务分解为若干有序任务组，同一组内的子任务可并行。
type Method struct {
	Name   string
	Pre    []Literal
	Groups [][]TaskExpr
}

// CompoundTask 是复合任务声明，可声明多种分解方式。
type CompoundTask struct {
	Name    string
	Params  []string
	Methods []Method
}

// Domain 是完整的领域描述。
type Domain struct {
	Name      string
	Actions   map[string]*Action
	Compounds map[string]*CompoundTask
}

func NewDomain(name string) *Domain {
	return &Domain{
		Name:      name,
		Actions:   make(map[string]*Action),
		Compounds: make(map[string]*CompoundTask),
	}
}

// GroundAction 是参数完全绑定后的可执行动作。
type GroundAction struct {
	ID   string
	Name string
	Args []string
	Pre  []Literal
	Add  []Literal
	Del  []Literal
}

func (g *GroundAction) Key() string {
	return g.Name + "(" + strings.Join(g.Args, ",") + ")"
}

// Reads 返回动作读取（前置条件依赖）的事实键集合。
func (g *GroundAction) Reads() []string {
	set := make(map[string]struct{})
	for _, lit := range g.Pre {
		set[atomKey(lit)] = struct{}{}
	}
	return sortedKeys(set)
}

// Writes 返回动作写入（增/删效果）的事实键集合。
func (g *GroundAction) Writes() []string {
	set := make(map[string]struct{})
	for _, lit := range g.Add {
		set[atomKey(lit)] = struct{}{}
	}
	for _, lit := range g.Del {
		set[atomKey(lit)] = struct{}{}
	}
	return sortedKeys(set)
}

// Validate 校验领域描述合法性，包括递归分解环检测。
func (d *Domain) Validate() error { return validateDomain(d) }

// ---- 内部实现 ----

// atomKey 返回事实/字面量的规范化键。
func atomKey(lit Literal) string {
	return lit.Predicate + "(" + strings.Join(lit.Args, ",") + ")"
}

// bind 将字面量中引用形参（"?x"）的参数替换为实参，常量保持不变。
func bind(lit Literal, args map[string]string) Literal {
	out := Literal{Predicate: lit.Predicate, Negated: lit.Negated, Args: make([]string, len(lit.Args))}
	for i, a := range lit.Args {
		if strings.HasPrefix(a, "?") {
			if v, ok := args[a]; ok {
				out.Args[i] = v
				continue
			}
		}
		out.Args[i] = a
	}
	return out
}

// bindAll 批量绑定字面量切片。
func bindAll(lits []Literal, args map[string]string) []Literal {
	out := make([]Literal, len(lits))
	for i, lit := range lits {
		out[i] = bind(lit, args)
	}
	return out
}

func newStateFromKeys(keys []string) *State {
	s := &State{atoms: make(map[string]struct{}, len(keys))}
	for _, k := range keys {
		s.atoms[k] = struct{}{}
	}
	return s
}

func (s *State) satisfies(lits []Literal) bool {
	for _, lit := range lits {
		if !s.Has(lit) {
			return false
		}
	}
	return true
}

// groundAction 将动作声明按位置形参绑定为基例动作。
func groundAction(a *Action, argv []string) *GroundAction {
	binding := make(map[string]string, len(a.Params))
	for i, p := range a.Params {
		binding[p] = argv[i]
	}
	return &GroundAction{
		Name: a.Name,
		Args: append([]string(nil), argv...),
		Pre:  bindAll(a.Pre, binding),
		Add:  bindAll(a.Add, binding),
		Del:  bindAll(a.Del, binding),
	}
}

// methodBinding 构造复合任务实参到方法形参的绑定表。
func methodBinding(params, argv []string) map[string]string {
	binding := make(map[string]string, len(params))
	for i, p := range params {
		binding[p] = argv[i]
	}
	return binding
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
