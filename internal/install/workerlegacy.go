package install

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/fleet"
)

// An installed worker heals in place. Every shape grove (or the operator) ever ran a worker in is
// replaced by the current one, and what the current shape supersedes is backed up and removed, so
// a re-run leaves one worker stack on the Mac, not two:
//
//   - a per-user LaunchAgent for com.grove.orchard-worker / com.grove.broker, in the installing
//     user's gui domain (grove before LaunchDaemons) or in another user's (a worker moved to a
//     dedicated account with `grove setup --user`);
//   - ~/.config/grove/run-worker.sh, the hand-written launcher that ran `orchard worker run`
//     without the wrapper or the guest-agent transport;
//   - ~/.config/grove/worker-bootstrap, the credential that launcher read, once the credential
//     file the wrapper reads holds the same token.

// processStart stamps the backup directory of every action in this process, so the install and the
// post-root finish step write to one directory.
var processStart = time.Now()

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return processStart
}

func (o Options) usersDir() string {
	if o.UsersDir != "" {
		return o.UsersDir
	}
	return "/Users"
}

func (o Options) lookupUID(name string) (int, bool) {
	if o.LookupUID != nil {
		return o.LookupUID(name)
	}
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	uid, err := strconv.Atoi(u.Uid)
	return uid, err == nil
}

// backupDir is where this run keeps copies of what it rewrites or removes.
func backupDir(opts Options) string {
	return filepath.Join(opts.ConfigDir(), "backups", opts.now().Format("20060102-150405"))
}

// backupSession copies files into backupDir and says so.
type backupSession struct {
	dir string
	out io.Writer
}

func newBackupSession(opts Options, out io.Writer) *backupSession {
	return &backupSession{dir: backupDir(opts), out: out}
}

// save copies path into the backup directory (0700 dir, 0600 files: it may hold a credential). A
// path that does not exist is not an error.
func (b *backupSession) save(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("back up %s: %w", path, err)
	}
	if err := os.MkdirAll(b.dir, 0o700); err != nil {
		return err
	}
	dest := filepath.Join(b.dir, filepath.Base(filepath.Dir(path))+"__"+filepath.Base(path))
	if err := os.WriteFile(dest, data, 0o600); err != nil {
		return fmt.Errorf("back up %s: %w", path, err)
	}
	fmt.Fprintf(b.out, "        backed up %s -> %s\n", path, dest)
	return nil
}

// legacyWorkerPlists are the installing user's per-user worker LaunchAgents.
func legacyWorkerPlists(opts Options) []string {
	m, _ := filepath.Glob(filepath.Join(opts.LaunchAgentsDir(), orchardWorkerLabel+"*.plist"))
	sort.Strings(m)
	return m
}

// legacyWorker is what the installing user still has of a worker shape the current one supersedes.
type legacyWorker struct {
	Plists []string // LaunchAgent plists (worker and broker)
	Script string   // run-worker.sh, or ""
	Token  string   // worker-bootstrap, or ""
}

func (l legacyWorker) empty() bool { return len(l.Plists) == 0 && l.Script == "" && l.Token == "" }

func (l legacyWorker) paths() []string {
	p := append([]string{}, l.Plists...)
	for _, f := range []string{l.Script, l.Token} {
		if f != "" {
			p = append(p, f)
		}
	}
	return p
}

func findLegacyWorker(opts Options) legacyWorker {
	l := legacyWorker{Plists: legacyWorkerPlists(opts)}
	if _, err := os.Lstat(legacyAgentPlist(opts, brokerLabel)); err == nil {
		l.Plists = append(l.Plists, legacyAgentPlist(opts, brokerLabel))
	}
	if _, err := os.Lstat(legacyWorkerScriptPath(opts)); err == nil {
		l.Script = legacyWorkerScriptPath(opts)
	}
	if _, err := os.Lstat(legacyWorkerBootstrapPath(opts)); err == nil {
		l.Token = legacyWorkerBootstrapPath(opts)
	}
	return l
}

func plistLabel(path string) string { return strings.TrimSuffix(filepath.Base(path), ".plist") }

// backupLegacyWorkerStep copies the superseded shape aside before anything replaces or removes it.
func backupLegacyWorkerStep(opts Options, bk *backupSession) Step {
	return Step{
		Name: "backup-legacy-worker",
		Description: fmt.Sprintf("Copy the superseded worker launch shape (per-user LaunchAgents, %s, %s) to %s before it is replaced.",
			legacyWorkerScriptPath(opts), legacyWorkerBootstrapPath(opts), bk.dir),
		Check: func(context.Context) (bool, error) { return findLegacyWorker(opts).empty(), nil },
		Apply: func(context.Context) error {
			for _, p := range findLegacyWorker(opts).paths() {
				if err := bk.save(p); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// retireLaunchAgentsStep removes what the current shape supersedes in the installing user's own
// session once the LaunchDaemon that replaces it is loaded: removing it first would leave the Mac
// with no worker until an admin runs the daemons step. The LaunchAgents themselves are also booted
// out by the daemons root script; this step finishes the files and any agent that script does not
// know by label. The legacy credential file goes only after the credential file the wrapper reads
// holds a token.
func retireLaunchAgentsStep(r Runner, opts Options, ds []daemon, bk *backupSession, out io.Writer) Step {
	return Step{
		Name:        "retire-launchagents",
		Description: "Unload and remove the per-user worker LaunchAgents, run-worker.sh and worker-bootstrap an earlier install left (backed up first), once the worker LaunchDaemon is loaded.",
		Check:       func(context.Context) (bool, error) { return findLegacyWorker(opts).empty(), nil },
		Apply: func(ctx context.Context) error {
			legacy := findLegacyWorker(opts)
			loaded := func(label string) bool {
				for _, d := range ds {
					if d.Label == label {
						return daemonLoaded(ctx, r, d)
					}
				}
				return daemonLoaded(ctx, r, ds[0]) // a launcher or extra agent of the worker
			}
			var remove []string
			for _, p := range legacy.Plists {
				if loaded(plistLabel(p)) {
					remove = append(remove, p)
				}
			}
			if loaded(ds[0].Label) {
				if legacy.Script != "" {
					remove = append(remove, legacy.Script)
				}
				if legacy.Token != "" {
					if secretFileIs(credentialPath(opts, workerBootstrapCredentialName), "") {
						remove = append(remove, legacy.Token)
					} else {
						fmt.Fprintf(out, "        keeping %s: its token is not in %s yet\n", legacy.Token, credentialPath(opts, workerBootstrapCredentialName))
					}
				}
			}
			if len(remove) == 0 && !legacy.empty() {
				fmt.Fprintf(out, "        the worker LaunchDaemon is not loaded yet; the superseded files stay until it is (re-run `grove install --role worker`)\n")
			}
			for _, p := range remove {
				if err := bk.save(p); err != nil {
					return err
				}
				if filepath.Ext(p) == ".plist" {
					_, _, _ = r.Run(ctx, "launchctl", "bootout", fmt.Sprintf("gui/%d/%s", opts.workerUID(), plistLabel(p))) // no GUI session is fine
				}
				if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
					return err
				}
				fmt.Fprintf(out, "        removed superseded %s\n", p)
			}
			return nil
		},
	}
}

// otherUserStack is a worker stack that another account on this Mac still runs as per-user
// LaunchAgents, for the worker identity this install now runs as LaunchDaemons.
type otherUserStack struct {
	User    string
	Plists  []string // worker + broker LaunchAgent plists
	Scripts []string // launcher scripts the worker plist runs, inside that user's home
}

var launcherScriptNames = map[string]bool{"run-worker.sh": true, workerWrapperName: true}

const workerWrapperName = "run-orchard-worker.zsh"

// findOtherUserStacks lists other accounts' worker LaunchAgents for the same worker. A stack whose
// plist names a different worker is another worker, and is left alone.
func findOtherUserStacks(opts Options, name string) []otherUserStack {
	entries, err := os.ReadDir(opts.usersDir())
	if err != nil {
		return nil
	}
	self := filepath.Clean(opts.homeDir())
	var out []otherUserStack
	for _, e := range entries {
		home := filepath.Join(opts.usersDir(), e.Name())
		if !e.IsDir() || filepath.Clean(home) == self || strings.HasPrefix(e.Name(), ".") || e.Name() == "Shared" {
			continue
		}
		agents := filepath.Join(home, "Library", "LaunchAgents")
		workers, _ := filepath.Glob(filepath.Join(agents, orchardWorkerLabel+"*.plist"))
		if len(workers) == 0 {
			continue
		}
		sort.Strings(workers)
		st := otherUserStack{User: e.Name()}
		for _, w := range workers {
			if id, ok := identityFromPlistFile(w); ok && id.Name != name {
				continue
			}
			st.Plists = append(st.Plists, w)
			st.Scripts = append(st.Scripts, plistLauncherScripts(w, home)...)
		}
		if len(st.Plists) == 0 {
			continue
		}
		if b := filepath.Join(agents, brokerLabel+".plist"); fileExists(b) {
			st.Plists = append(st.Plists, b)
		}
		out = append(out, st)
	}
	return out
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// plistLauncherScripts are the launcher scripts a worker LaunchAgent runs from inside home.
func plistLauncherScripts(plist, home string) []string {
	b, err := os.ReadFile(plist)
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range plistStringRE.FindAllStringSubmatch(string(b), -1) {
		t := m[1]
		if filepath.IsAbs(t) && launcherScriptNames[filepath.Base(t)] && strings.HasPrefix(t, filepath.Clean(home)+string(filepath.Separator)) {
			out = append(out, t)
		}
	}
	return out
}

// retireOtherUserStacksStep removes other accounts' superseded worker stacks. Another account's
// files and launchd domain are not the installing user's to touch, so it is a root step; it runs
// after the daemons step, so the worker is never without a stack.
func retireOtherUserStacksStep(r Runner, opts Options, name string) Step {
	stacks := findOtherUserStacks(opts, name)
	dir := backupDir(opts)
	worker, _ := opts.workerUser()
	root := func(context.Context) (string, error) {
		if worker == "" || worker == "root" {
			return "", fmt.Errorf("the worker user must not be root")
		}
		var b strings.Builder
		fmt.Fprintf(&b, "/bin/mkdir -p %s\n", shQuote(dir))
		for _, st := range stacks {
			for _, p := range st.Plists {
				fmt.Fprintf(&b, "/bin/cp -p %s %s\n", shQuote(p), shQuote(filepath.Join(dir, st.User+"__"+filepath.Base(p))))
				if uid, ok := opts.lookupUID(st.User); ok {
					fmt.Fprintf(&b, "/bin/launchctl bootout gui/%d/%s 2>/dev/null || true\n", uid, plistLabel(p))
				}
				fmt.Fprintf(&b, "/bin/rm -f %s\n", shQuote(p))
			}
			for _, s := range st.Scripts {
				fmt.Fprintf(&b, "/bin/cp -p %s %s\n", shQuote(s), shQuote(filepath.Join(dir, st.User+"__"+filepath.Base(s))))
				fmt.Fprintf(&b, "/bin/rm -f %s\n", shQuote(s))
			}
		}
		fmt.Fprintf(&b, "/usr/sbin/chown -R %s %s\n/bin/chmod -R go-rwx %s\n", shQuote(worker), shQuote(filepath.Join(opts.ConfigDir(), "backups")), shQuote(dir))
		return b.String(), nil
	}
	var who []string
	for _, st := range stacks {
		who = append(who, st.User)
	}
	return Step{
		Name:       "retire-other-user-stacks",
		Privileged: true,
		Description: fmt.Sprintf("Unload and remove the per-user worker LaunchAgents %s still has for this worker (backed up to %s), now that the LaunchDaemons replace them.",
			strings.Join(who, ", "), dir),
		Root: root,
		Check: func(context.Context) (bool, error) {
			return len(findOtherUserStacks(opts, name)) == 0, nil
		},
		Apply: func(ctx context.Context) error { return runRootText(ctx, r, root) },
	}
}

// legacyShapeProblems lists what is wrong with the worker launch shape on this Mac, for
// `grove doctor`. Empty means it is the current shape.
func legacyShapeProblems(opts Options) []string {
	var problems []string
	l := findLegacyWorker(opts)
	if l.Script != "" {
		problems = append(problems, l.Script+" launches Orchard directly, without the wrapper or the guest-agent transport (VMs boot-loop with \"no route to host\")")
	}
	if l.Token != "" {
		problems = append(problems, l.Token+" is a legacy credential file")
	}
	if len(l.Plists) > 0 {
		problems = append(problems, "per-user LaunchAgent(s) "+strings.Join(baseNames(l.Plists), ", ")+" would run a second worker")
	}
	if b, err := os.ReadFile(workerWrapperPath(opts)); err == nil && !strings.Contains(string(b), "--guest-agent-transport") {
		problems = append(problems, workerWrapperPath(opts)+" lacks --guest-agent-transport")
	}
	for _, dir := range []string{opts.launchDaemonsDir(), opts.LaunchAgentsDir()} {
		m, _ := filepath.Glob(filepath.Join(dir, "com.grove.*.plist"))
		for _, p := range m {
			if prog := plistProgram(p); prog != "" && !filepath.IsAbs(prog) {
				problems = append(problems, p+" runs the bare program "+prog+", which launchd cannot find")
			}
		}
	}
	if id := detectWorkerIdentity(opts); id.Name != "" {
		for _, st := range findOtherUserStacks(opts, id.Name) {
			problems = append(problems, "user "+st.User+" still has a worker LaunchAgent stack for "+id.Name)
		}
	}
	return problems
}

func baseNames(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Base(p)
	}
	return out
}

// plistProgram is the first ProgramArguments entry of a plist, or "".
func plistProgram(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(b)
	i := strings.Index(text, "<key>ProgramArguments</key>")
	if i < 0 {
		return ""
	}
	if m := plistStringRE.FindStringSubmatch(text[i:]); m != nil {
		return m[1]
	}
	return ""
}

// checkWorkerShape flags a worker launched in a shape the current installer replaces, or whose
// identity no longer matches fleet.yaml, and points at the one command that heals it.
func checkWorkerShape(ctx context.Context, opts Options, cfg *config.Config) CheckResult {
	const name = "worker-shape"
	const fix = "re-run `grove install --role worker`: it keeps the worker's name and labels, replaces the old launch shape and removes what it supersedes"
	if problems := legacyShapeProblems(opts); len(problems) > 0 {
		return CheckResult{Name: name, OK: false, Detail: strings.Join(problems, "; "), Remediation: fix}
	}
	if cfg == nil || cfg.Fleet == "" {
		return CheckResult{Name: name, OK: true, Detail: "current launch shape"}
	}
	spec, err := fleet.Load(cfg.Fleet)
	if err != nil {
		return CheckResult{Name: name, OK: true, Detail: "current launch shape (fleet.yaml not readable here, selectors not checked)"}
	}
	c, err := opts.orchardClient(cfg.Orchard)
	if err != nil {
		return CheckResult{Name: name, OK: true, Detail: "current launch shape (Orchard not reachable, selectors not checked)"}
	}
	ctx, cancel := context.WithTimeout(ctx, httpCheckTimeout)
	defer cancel()
	workers, err := c.ListWorkers(ctx)
	if err != nil {
		return CheckResult{Name: name, OK: true, Detail: "current launch shape (Orchard not reachable, selectors not checked)"}
	}
	var orphaned []string
	for _, p := range spec.Pools {
		if len(p.WorkerSelector) == 0 {
			continue
		}
		matched := false
		for _, w := range workers {
			if selectorMatches(w.Labels, p.WorkerSelector) {
				matched = true
			}
		}
		if !matched {
			orphaned = append(orphaned, p.Name)
		}
	}
	if len(orphaned) > 0 {
		id := detectWorkerIdentity(opts)
		return CheckResult{Name: name, OK: false,
			Detail: fmt.Sprintf("no Orchard worker matches the workerSelector of pool(s) %s; this Mac's worker is %q with labels %q",
				strings.Join(orphaned, ", "), id.Name, id.Labels),
			Remediation: "re-run `grove install --role worker --name <the name the selector expects> [--labels ...]` on this Mac (on the control plane host it also rewrites fleet.yaml and deletes the old worker record)"}
	}
	return CheckResult{Name: name, OK: true, Detail: "current launch shape; every pool's workerSelector matches a worker"}
}

func selectorMatches(labels, selector map[string]string) bool {
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}
