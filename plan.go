package taskplan

// Step is one primitive action occurrence in a Plan. Agenda is the list
// of tasks that remained (including this step's own task) at the moment
// the planner emitted the step; the executor uses it to replan the
// not-yet-executed remainder without repeating finished work.
type Step struct {
	// Index is the step's position in the total execution order.
	Index int `json:"index"`
	// Action is the name of the primitive action to execute.
	Action string `json:"action"`
	// GoalIndex identifies the top-level goal the step serves.
	GoalIndex int `json:"goalIndex"`
	// TaskID is the unique id of the task occurrence this step expands.
	TaskID uint64 `json:"taskId"`
	// Agenda holds the remaining tasks (this step's task first) as of
	// this step; it is the replanning seed after a deviation.
	Agenda []TaskRef `json:"agenda"`
}

// Plan is the output of the Planner: a deterministic total order of
// primitive steps plus a partition into Layers of mutually independent
// steps that may execute concurrently.
type Plan struct {
	// Steps is the full action sequence in execution order.
	Steps []Step `json:"steps"`
	// Layers partitions step indices so that every step in a layer only
	// depends on steps of earlier layers. Steps inside one layer have
	// no dependency and no conflicting effects and may run in parallel.
	Layers [][]int `json:"layers"`
}

// Actions returns the action names of the plan in execution order.
func (p *Plan) Actions() []string {
	out := make([]string, len(p.Steps))
	for i, s := range p.Steps {
		out[i] = s.Action
	}
	return out
}

// computeLayers partitions steps into parallel layers. A step is placed
// in the earliest layer such that every conflicting earlier step lies in
// a strictly earlier layer. The result is deterministic for a
// deterministic step order.
func computeLayers(steps []Step, domain *Domain) [][]int {
	layerOf := make([]int, len(steps))
	maxLayer := -1
	for i := range steps {
		layer := 0
		for j := 0; j < i; j++ {
			if conflicts(domain.Actions[steps[j].Action], domain.Actions[steps[i].Action]) && layerOf[j] >= layer {
				layer = layerOf[j] + 1
			}
		}
		layerOf[i] = layer
		if layer > maxLayer {
			maxLayer = layer
		}
	}
	layers := make([][]int, maxLayer+1)
	for i := range layers {
		layers[i] = []int{}
	}
	for i, l := range layerOf {
		layers[l] = append(layers[l], i)
	}
	return layers
}

// conflicts reports whether actions a and b (a ordered before b) cannot
// belong to the same parallel layer: either one changes a fact the other
// relies on (positively or negatively), or their effects oppose each
// other. Same-layer actions must commute and stay individually
// applicable regardless of execution order.
func conflicts(a, b *Action) bool {
	if intersects(a.Eff.Add, b.Pre.Pos) || intersects(a.Eff.Del, b.Pre.Pos) ||
		intersects(a.Eff.Add, b.Pre.Neg) || intersects(a.Eff.Del, b.Pre.Neg) {
		return true
	}
	if intersects(b.Eff.Add, a.Pre.Pos) || intersects(b.Eff.Del, a.Pre.Pos) ||
		intersects(b.Eff.Add, a.Pre.Neg) || intersects(b.Eff.Del, a.Pre.Neg) {
		return true
	}
	if intersects(a.Eff.Add, b.Eff.Del) || intersects(a.Eff.Del, b.Eff.Add) {
		return true
	}
	return false
}

// intersects reports whether the two fact lists share any element.
func intersects(xs, ys []string) bool {
	if len(xs) == 0 || len(ys) == 0 {
		return false
	}
	set := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		set[x] = struct{}{}
	}
	for _, y := range ys {
		if _, ok := set[y]; ok {
			return true
		}
	}
	return false
}
