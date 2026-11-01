package install

import (
	"encoding/json"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A worker's identity is the Orchard name it registers under and the labels it advertises.
// fleet.yaml's workerSelector matches the labels, and the bootstrap credential is minted for the
// name, so re-running `grove install --role worker` must not change either by accident: an
// installer that renamed a Mac to its hostname orphaned every pool that selected the old label.
//
// The identity is, in order of authority: --name/--labels, the record this installer writes
// (worker-identity.json), the wrapper this installer renders, then the launch shapes that predate
// the installer (a hand-written run-worker.sh, a LaunchAgent with inline arguments), and finally
// os.Hostname() for a Mac with no worker.

// workerIdentity is a worker's name and labels, and where they were found.
type workerIdentity struct {
	Name   string `json:"name"`
	Labels string `json:"labels"` // Orchard's --labels form: "k=v,k=v"
	// Pending lists identities this worker held before a rename that is not finished: the old
	// Orchard worker record is still to delete and fleet.yaml selectors still to rewrite. They are
	// recorded so a rename the admin completes in a later run, or that failed halfway, is retried.
	Pending []pendingIdentity `json:"pending,omitempty"`

	Controller string `json:"-"` // the controller URL found beside it, if any
	Source     string `json:"-"` // human-readable: where this identity came from
}

type pendingIdentity struct {
	Name   string `json:"name"`
	Labels string `json:"labels"`
}

func workerIdentityPath(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "worker-identity.json")
}

// legacyWorkerScriptPath is the hand-written launcher a worker that predates the installer ran:
// `orchard worker run --name N --labels L <controller>` with no wrapper.
func legacyWorkerScriptPath(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "run-worker.sh")
}

// legacyWorkerBootstrapPath is where that launcher read its bootstrap credential from.
func legacyWorkerBootstrapPath(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "worker-bootstrap")
}

// readWorkerIdentityRecord reads the identity the installer recorded; ok is false if there is none.
func readWorkerIdentityRecord(opts Options) (workerIdentity, bool) {
	b, err := os.ReadFile(workerIdentityPath(opts))
	if err != nil {
		return workerIdentity{}, false
	}
	var id workerIdentity
	if json.Unmarshal(b, &id) != nil || id.Name == "" {
		return workerIdentity{}, false
	}
	id.Source = workerIdentityPath(opts)
	return id, true
}

func renderWorkerIdentityRecord(id workerIdentity) string {
	b, _ := json.MarshalIndent(workerIdentity{Name: id.Name, Labels: id.Labels, Pending: id.Pending}, "", "  ")
	return string(b) + "\n"
}

// detectWorkerIdentity finds the identity of the worker already installed on this Mac, or the zero
// value if there is none. It reads only the installing user's own files.
func detectWorkerIdentity(opts Options) workerIdentity {
	if id, ok := readWorkerIdentityRecord(opts); ok {
		fillController(&id, opts)
		return id
	}
	if b, err := os.ReadFile(workerWrapperPath(opts)); err == nil {
		if id, ok := identityFromText(string(b)); ok {
			id.Source = workerWrapperPath(opts)
			return id
		}
	}
	if b, err := os.ReadFile(legacyWorkerScriptPath(opts)); err == nil {
		if id, ok := identityFromText(string(b)); ok {
			id.Source = legacyWorkerScriptPath(opts)
			return id
		}
	}
	for _, plist := range legacyWorkerPlists(opts) {
		if id, ok := identityFromPlistFile(plist); ok {
			return id
		}
	}
	return workerIdentity{}
}

// fillController adds the controller URL of whatever launcher is installed, which the identity
// record does not carry.
func fillController(id *workerIdentity, opts Options) {
	if b, err := os.ReadFile(workerWrapperPath(opts)); err == nil {
		if got, ok := identityFromText(string(b)); ok {
			id.Controller = got.Controller
			return
		}
	}
	if b, err := os.ReadFile(legacyWorkerScriptPath(opts)); err == nil {
		if got, ok := identityFromText(string(b)); ok {
			id.Controller = got.Controller
		}
	}
}

var plistStringRE = regexp.MustCompile(`<string>([^<]*)</string>`)

// identityFromPlistFile reads a LaunchAgent's arguments. A plist that runs a launcher script instead
// of Orchard itself is followed into that script.
func identityFromPlistFile(path string) (workerIdentity, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return workerIdentity{}, false
	}
	text := string(b)
	if i := strings.Index(text, "<key>ProgramArguments</key>"); i >= 0 {
		text = text[i:]
		if j := strings.Index(text, "</array>"); j >= 0 {
			text = text[:j]
		}
	}
	var tokens []string
	for _, m := range plistStringRE.FindAllStringSubmatch(text, -1) {
		tokens = append(tokens, html.UnescapeString(m[1]))
	}
	if id, ok := identityFromTokens(tokens); ok {
		id.Source = path
		return id, true
	}
	for _, t := range tokens {
		if !filepath.IsAbs(t) {
			continue
		}
		if sb, err := os.ReadFile(t); err == nil {
			if id, ok := identityFromText(string(sb)); ok {
				id.Source = path + " -> " + t
				return id, true
			}
		}
	}
	return workerIdentity{}, false
}

// identityFromText reads `orchard worker run ...` out of a shell script or wrapper.
func identityFromText(text string) (workerIdentity, bool) {
	return identityFromTokens(shellTokens(text))
}

// shellTokens splits text the way a shell would for the purpose of finding flags: whitespace
// separated, single and double quotes honoured, backslash-newline joined, comment lines dropped.
func shellTokens(text string) []string {
	text = strings.ReplaceAll(text, "\\\n", " ")
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "#") {
			continue
		}
		lines = append(lines, l)
	}
	text = strings.Join(lines, " ")

	var tokens []string
	var cur strings.Builder
	inTok := false
	var quote rune
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote, inTok = c, true
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			if inTok {
				tokens = append(tokens, cur.String())
				cur.Reset()
				inTok = false
			}
		default:
			cur.WriteRune(c)
			inTok = true
		}
	}
	if inTok {
		tokens = append(tokens, cur.String())
	}
	return tokens
}

// identityFromTokens finds the first `worker run` and reads --name, --labels and the controller URL
// after it. ok is false when there is no --name: such a worker registers under Orchard's default.
func identityFromTokens(tokens []string) (workerIdentity, bool) {
	start := -1
	for i := 0; i+1 < len(tokens); i++ {
		if tokens[i] == "worker" && tokens[i+1] == "run" {
			start = i + 2
			break
		}
	}
	if start < 0 {
		return workerIdentity{}, false
	}
	var id workerIdentity
	var labels []string
	for i := start; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case t == "--name" && i+1 < len(tokens):
			i++
			id.Name = tokens[i]
		case strings.HasPrefix(t, "--name="):
			id.Name = strings.TrimPrefix(t, "--name=")
		case t == "--labels" && i+1 < len(tokens):
			i++
			labels = append(labels, tokens[i])
		case strings.HasPrefix(t, "--labels="):
			labels = append(labels, strings.TrimPrefix(t, "--labels="))
		case strings.HasPrefix(t, "http://") || strings.HasPrefix(t, "https://"):
			if id.Controller == "" {
				id.Controller = t
			}
		}
	}
	id.Labels = strings.Join(labels, ",")
	return id, id.Name != ""
}

// parseLabels reads Orchard's "k=v,k=v" label form.
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if ok && k != "" {
			out[k] = v
		}
	}
	return out
}

// formatLabels is parseLabels's inverse, in a stable order.
func formatLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + m[k]
	}
	return strings.Join(parts, ",")
}

// defaultWorkerLabels is what a new worker advertises.
func defaultWorkerLabels(name string) string { return "host=" + name + ",arch=arm64" }

// chooseWorkerIdentity resolves the identity this install renders: explicit flags first, then the
// installed identity, then os.Hostname(). A rename that gives no --labels carries the installed
// labels over, with any label whose value was the old name pointed at the new one, so the pools
// that selected host=<old> select host=<new>.
func chooseWorkerIdentity(opts Options, prev workerIdentity) workerIdentity {
	chosen := workerIdentity{Name: opts.Hostname, Labels: opts.Labels, Source: "flags"}
	if chosen.Name == "" {
		if prev.Name != "" {
			chosen.Name, chosen.Source = prev.Name, prev.Source
		} else {
			chosen.Name, chosen.Source = opts.hostname(), "this Mac's hostname"
		}
	}
	if chosen.Labels == "" {
		switch {
		case prev.Name == "" || prev.Labels == "":
			chosen.Labels = defaultWorkerLabels(chosen.Name)
		case prev.Name == chosen.Name:
			chosen.Labels = prev.Labels
		default:
			parts := strings.Split(prev.Labels, ",")
			for i, p := range parts {
				if k, v, ok := strings.Cut(strings.TrimSpace(p), "="); ok && v == prev.Name {
					parts[i] = k + "=" + chosen.Name
				}
			}
			chosen.Labels = strings.Join(parts, ",")
		}
	}
	chosen.Controller = prev.Controller
	chosen.Pending = prev.Pending
	return chosen
}

// labelChange is one label whose value differs between two identities.
type labelChange struct{ Key, Old, New string }

// selectorChanges are the workerSelector entries that stop matching when a worker goes from old
// labels to new: every label key whose value changed or disappeared.
func selectorChanges(oldLabels, newLabels string) []labelChange {
	o, n := parseLabels(oldLabels), parseLabels(newLabels)
	var out []labelChange
	for k, ov := range o {
		if nv := n[k]; nv != ov {
			out = append(out, labelChange{Key: k, Old: ov, New: nv})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// renameFleetNote is the exact fleet.yaml change a rename needs, for an operator to make by hand.
func renameFleetNote(prev, chosen workerIdentity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "worker %q (%s) becomes %q (%s).\n", prev.Name, prev.Labels, chosen.Name, chosen.Labels)
	changes := selectorChanges(prev.Labels, chosen.Labels)
	if len(changes) == 0 {
		b.WriteString("  fleet.yaml: no workerSelector changes (the labels are the same).\n")
	}
	for _, c := range changes {
		if c.New == "" {
			fmt.Fprintf(&b, "  fleet.yaml: every workerSelector entry `%s: %s` stops matching; give the worker that label with --labels, or change the selector.\n", c.Key, c.Old)
			continue
		}
		fmt.Fprintf(&b, "  fleet.yaml: in every pool's workerSelector, change `%s: %s` to `%s: %s`.\n", c.Key, c.Old, c.Key, c.New)
	}
	if prev.Name != chosen.Name {
		fmt.Fprintf(&b, "  Orchard: delete the old worker record %q on the control plane.\n", prev.Name)
	}
	return b.String()
}
