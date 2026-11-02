package conformance

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A trace in the Informal Trace Format (https://apalache-mc.org/docs/adr/015adr-trace.html), as
// `quint run --out-itf` writes it. Only the value forms grove's models use are decoded: booleans,
// strings, integers, sets and maps.
type itfTrace struct {
	file   string
	states []itfState
}

type itfState map[string]any

func loadITF(path string) (itfTrace, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return itfTrace{}, err
	}
	var doc struct {
		States []map[string]json.RawMessage `json:"states"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return itfTrace{}, fmt.Errorf("%s: %w", path, err)
	}
	tr := itfTrace{file: filepath.Base(path)}
	for i, st := range doc.States {
		s := itfState{}
		for k, v := range st {
			if strings.HasPrefix(k, "#") || strings.HasPrefix(k, "mbt::") {
				continue
			}
			// "snoozeMBT::snooze::bit" → "bit"
			name := k
			if i := strings.LastIndex(k, "::"); i >= 0 {
				name = k[i+2:]
			}
			val, err := decodeITF(v)
			if err != nil {
				return itfTrace{}, fmt.Errorf("%s state %d %s: %w", path, i, k, err)
			}
			s[name] = val
		}
		tr.states = append(tr.states, s)
	}
	return tr, nil
}

// decodeITF returns bool, string, int64, []any (a set, in ITF order) or map[string]any.
func decodeITF(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return itfValue(v)
}

func itfValue(v any) (any, error) {
	switch x := v.(type) {
	case bool, string:
		return x, nil
	case float64:
		return int64(x), nil
	case map[string]any:
		if b, ok := x["#bigint"].(string); ok {
			n, ok := new(big.Int).SetString(b, 10)
			if !ok || !n.IsInt64() {
				return nil, fmt.Errorf("bigint %q", b)
			}
			return n.Int64(), nil
		}
		if set, ok := x["#set"].([]any); ok {
			out := make([]any, 0, len(set))
			for _, e := range set {
				d, err := itfValue(e)
				if err != nil {
					return nil, err
				}
				out = append(out, d)
			}
			return out, nil
		}
		if pairs, ok := x["#map"].([]any); ok {
			out := map[string]any{}
			for _, p := range pairs {
				kv, ok := p.([]any)
				if !ok || len(kv) != 2 {
					return nil, fmt.Errorf("map entry %v", p)
				}
				k, ok := kv[0].(string)
				if !ok {
					return nil, fmt.Errorf("map key %v", kv[0])
				}
				d, err := itfValue(kv[1])
				if err != nil {
					return nil, err
				}
				out[k] = d
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("unsupported ITF value %v", v)
}

func (s itfState) bool(name string) bool { return s[name].(bool) }
func (s itfState) int(name string) int64 { return s[name].(int64) }
func (s itfState) strMap(name string) map[string]string {
	out := map[string]string{}
	for k, v := range s[name].(map[string]any) {
		out[k] = v.(string)
	}
	return out
}
func (s itfState) strSet(name string) []string {
	var out []string
	for _, v := range s[name].([]any) {
		out = append(out, v.(string))
	}
	sort.Strings(out)
	return out
}

// tracesIn loads every *.itf.json in dir, in name order.
func tracesIn(dir string) ([]itfTrace, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.itf.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	var out []itfTrace
	for _, p := range paths {
		tr, err := loadITF(p)
		if err != nil {
			return nil, err
		}
		out = append(out, tr)
	}
	return out, nil
}
