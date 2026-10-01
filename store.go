package taskplanner

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
)

// storedAction 是已执行/在途动作的持久化记录。
type storedAction struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Args   []string  `json:"args"`
	Status string    `json:"status"`
	Pre    []Literal `json:"pre,omitempty"`
	Add    []Literal `json:"add,omitempty"`
	Del    []Literal `json:"del,omitempty"`
}

// storedGroup 是任务网络中一个任务组的持久化形式。
type storedGroup struct {
	Tasks []storedTask `json:"tasks"`
}

type storedTask struct {
	Name string   `json:"name"`
	Args []string `json:"args"`
	Path string   `json:"path"`
}

// journal 是一次规划执行的持久化进度快照。
type journal struct {
	PlanID     string           `json:"plan_id"`
	Request    PlanRequest      `json:"request"`
	StateAtoms []string         `json:"state_atoms"`
	Network    []storedGroup    `json:"network"`
	InFlight   []storedAction   `json:"in_flight,omitempty"`
	HeadWaves  [][]storedAction `json:"head_waves,omitempty"`
	HeadIndex  int              `json:"head_index,omitempty"`
	Banned     []string         `json:"banned,omitempty"`
	Replans    int              `json:"replans"`
	Status     string           `json:"status"`
}

// store 负责 journal 的原子落盘与读取。
type store struct {
	dir string
}

// errNoJournal 表示不存在任何已提交的进度。
var errNoJournal = errors.New("no committed journal found")

const (
	journalWaveStarted   = "wave_started"
	journalWaveCommitted = "wave_committed"
	journalComplete      = "complete"
)

func newStore(dir string) *store { return &store{dir: dir} }

func (s *store) path(planID string) string {
	return filepath.Join(s.dir, planID+".json")
}

// save 将进度快照原子写入磁盘（临时文件 + rename）。
func (s *store) save(j *journal) error {
	if s.dir == "" {
		return nil
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".journal-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.path(j.PlanID))
}

func (s *store) load(planID string) (*journal, error) {
	if s.dir == "" {
		return nil, errNoJournal
	}
	data, err := os.ReadFile(s.path(planID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNoJournal
	}
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return &j, nil
}

// latest 返回目录中最近修改的 journal（时间戳相同时按文件名排序保证确定性）。
func (s *store) latest() (*journal, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errNoJournal
		}
		return nil, err
	}
	type candidate struct {
		id string
		fi os.FileInfo
	}
	var cands []candidate
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			return nil, err
		}
		cands = append(cands, candidate{id: e.Name()[:len(e.Name())-len(".json")], fi: fi})
	}
	if len(cands) == 0 {
		return nil, errNoJournal
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].fi.ModTime().Equal(cands[j].fi.ModTime()) {
			return cands[i].id < cands[j].id
		}
		return cands[i].fi.ModTime().After(cands[j].fi.ModTime())
	})
	return s.load(cands[0].id)
}

// ---- journal 与内存结构互转 ----

func networkToStored(net *taskNetwork) []storedGroup {
	if net == nil {
		return nil
	}
	out := make([]storedGroup, len(net.groups))
	for gi, group := range net.groups {
		st := storedGroup{Tasks: make([]storedTask, len(group))}
		for ti, t := range group {
			st.Tasks[ti] = storedTask{Name: t.name, Args: append([]string(nil), t.args...), Path: t.path}
		}
		out[gi] = st
	}
	return out
}

func storedToNetwork(groups []storedGroup) *taskNetwork {
	net := &taskNetwork{groups: make([][]groundTask, len(groups))}
	for gi, g := range groups {
		net.groups[gi] = make([]groundTask, len(g.Tasks))
		for ti, t := range g.Tasks {
			net.groups[gi][ti] = groundTask{name: t.Name, args: append([]string(nil), t.Args...), path: t.Path}
		}
	}
	return net
}

func waveToStoredActions(w *Wave, status string) []storedAction {
	out := make([]storedAction, len(w.Actions))
	for i, g := range w.Actions {
		out[i] = storedAction{
			ID:     g.ID,
			Name:   g.Name,
			Args:   append([]string(nil), g.Args...),
			Status: status,
			Pre:    append([]Literal(nil), g.Pre...),
			Add:    append([]Literal(nil), g.Add...),
			Del:    append([]Literal(nil), g.Del...),
		}
	}
	return out
}

func storedToWave(actions []storedAction) *Wave {
	acts := make([]*GroundAction, len(actions))
	for i, a := range actions {
		acts[i] = &GroundAction{
			ID:   a.ID,
			Name: a.Name,
			Args: append([]string(nil), a.Args...),
			Pre:  append([]Literal(nil), a.Pre...),
			Add:  append([]Literal(nil), a.Add...),
			Del:  append([]Literal(nil), a.Del...),
		}
	}
	return &Wave{Actions: acts}
}

func wavesToStored(waves []*Wave) [][]storedAction {
	if waves == nil {
		return nil
	}
	out := make([][]storedAction, len(waves))
	for i, w := range waves {
		out[i] = waveToStoredActions(w, "planned")
	}
	return out
}

// wavesToStoredAll 返回 waves[startIdx:] 的持久化形式，用于部分完成组的恢复。
func wavesToStoredFrom(all [][]storedAction, startIdx int) [][]storedAction {
	if startIdx >= len(all) {
		return nil
	}
	out := make([][]storedAction, len(all)-startIdx)
	copy(out, all[startIdx:])
	return out
}

func bannedToSlice(banned map[string]struct{}) []string {
	out := make([]string, 0, len(banned))
	for k := range banned {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sliceToBanned(keys []string) map[string]struct{} {
	out := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		out[k] = struct{}{}
	}
	return out
}
