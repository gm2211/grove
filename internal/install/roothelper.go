package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The root helper: the one root LaunchDaemon that does a worker Mac's root steps from then on, so
// an admin types a password once. `sudo grove root-helper install --user <worker user>` installs
// it; afterwards the worker user's own grove asks it for a step by writing a request, and the
// helper (root) does it if the request passes its checks.
//
// The helper never runs new code: its binary is copied once, from the admin's grove, into a
// root-owned directory, and never updates itself. That matters because every grove release is
// built from main by this repository's workflow, and the worker user may hold a GitHub login that
// can put a commit on main; a root process that trusted "a verified release" would trust them.
// What a request may ask is data the helper checks against a fixed grammar (pfgrammar.go), chosen
// so that the worst a request can do is loosen the fence around the user's own VMs, which the
// user could already do by running a VM without it. Request kinds today: the VM firewall's rules.
// A grove that needs a new kind of root step needs the helper reinstalled (the helper refuses a
// request version it doesn't know, and `grove doctor` names the command).
//
// Files (rootHelperHome, root-owned 0755):
//
//	bin/grove        the helper's binary (root, 0755)
//	config.json      the worker user's name and uid (root, 0644)
//	inbox/           the worker user's (0700): request.json goes here
//	state/           root's (0700): the last rules that loaded
//	status.json      what the helper last did (root, 0644), read by the user's grove and doctor

const (
	RootHelperLabel      = "com.grove.root-helper"
	rootHelperVersion    = 1 // the request version this helper reads
	rootHelperMaxRequest = 64 << 10
	rootHelperLog        = "/Library/Logs/grove-root-helper.log"
)

// Package variables so tests can stand in for root's paths and checks.
var (
	rootHelperHome = "/Library/Application Support/grove/root-helper"
	// rootHelperOwnerOK reports whether a file's owner is root's: the status and config the user's
	// grove reads must be root's, or a user process could fake them.
	rootHelperOwnerOK = func(st *syscall.Stat_t) bool { return st.Uid == 0 }
	// rootHelperEUID is os.Geteuid; tests stand in for root.
	rootHelperEUID = os.Geteuid
	// rootHelperPoll and rootHelperWait pace a request's wait for the helper's answer.
	rootHelperPoll = time.Second
	rootHelperWait = 90 * time.Second
)

func rootHelperBin() string        { return filepath.Join(rootHelperHome, "bin", "grove") }
func rootHelperConfigPath() string { return filepath.Join(rootHelperHome, "config.json") }
func rootHelperInbox() string      { return filepath.Join(rootHelperHome, "inbox") }
func rootHelperRequest() string    { return filepath.Join(rootHelperInbox(), "request.json") }
func rootHelperState() string      { return filepath.Join(rootHelperHome, "state") }
func rootHelperLastGood() string   { return filepath.Join(rootHelperState(), "vm-firewall.rules") }
func rootHelperStatusPath() string { return filepath.Join(rootHelperHome, "status.json") }

// RootHelperBin is the helper's binary, for the CLI's checks.
func RootHelperBin() string { return rootHelperBin() }

// RootHelperConfig pins the one user the helper takes requests from.
type RootHelperConfig struct {
	Version int    `json:"version"`
	User    string `json:"user"`
	UID     uint32 `json:"uid"`
}

// RootHelperRequest is what the worker user's grove asks for. Unknown fields are refused.
type RootHelperRequest struct {
	Version  int                   `json:"version"`
	Firewall *RootHelperFirewallV1 `json:"firewall,omitempty"`
}

// RootHelperFirewallV1 is the VM firewall's rules, inside pfgrammar.go's grammar.
type RootHelperFirewallV1 struct {
	Rules string `json:"rules"`
}

// RootHelperStatus is what the helper last did. It never echoes a request's text.
type RootHelperStatus struct {
	Version       int       `json:"version"`
	Accepts       []int     `json:"accepts"`
	GroveVersion  string    `json:"groveVersion"`
	User          string    `json:"user"`
	At            time.Time `json:"at"`
	RulesSHA256   string    `json:"rulesSHA256,omitempty"`   // the rules loaded now
	Loaded        bool      `json:"loaded"`                  // the anchor is loaded and pf on
	RequestSHA256 string    `json:"requestSHA256,omitempty"` // the rules the request asked for
	Error         string    `json:"error,omitempty"`
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// --- the user's side ---

// RootHelperFor returns the helper's config when a helper is installed for opts' worker user.
func RootHelperFor(opts Options) (RootHelperConfig, bool) {
	name, err := opts.workerUser()
	if err != nil {
		return RootHelperConfig{}, false
	}
	cfg, err := readRootHelperConfig()
	if err != nil || cfg.User != name {
		return RootHelperConfig{}, false
	}
	return cfg, true
}

func readRootHelperConfig() (RootHelperConfig, error) {
	var cfg RootHelperConfig
	b, err := readRootOwned(rootHelperConfigPath(), rootHelperMaxRequest)
	if err != nil {
		return cfg, err
	}
	if err := strictJSON(b, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", rootHelperConfigPath(), err)
	}
	if cfg.Version != 1 || cfg.User == "" || cfg.User == "root" || cfg.UID == 0 {
		return cfg, fmt.Errorf("%s does not name an ordinary worker user", rootHelperConfigPath())
	}
	return cfg, nil
}

// ReadRootHelperStatus reads the helper's status.json, which must be root's.
func ReadRootHelperStatus() (RootHelperStatus, error) {
	var st RootHelperStatus
	b, err := readRootOwned(rootHelperStatusPath(), rootHelperMaxRequest)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

// readRootOwned reads a regular file, not a symlink, owned by root and writable by nobody else.
func readRootOwned(path string, max int64) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || !rootHelperOwnerOK(st) || info.Mode().Perm()&0o022 != 0 {
		return nil, fmt.Errorf("%s is not a root-owned file", path)
	}
	return io.ReadAll(io.LimitReader(f, max))
}

func strictJSON(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data after the JSON object")
	}
	return nil
}

// helperFirewallSteps is the VM firewall on a Mac with the root helper: one ordinary step, no
// sudo. Its check is the helper's status (the rules grove renders now are the ones loaded); its
// apply sends them to the helper and waits for its answer.
func helperFirewallSteps(r Runner, opts Options) []Step {
	return []Step{{
		Name:        "host-firewall",
		Description: "Have the root helper (" + RootHelperLabel + ") load the VM firewall, so VMs reach only DHCP, DNS and the broker on this Mac, and Nomad RPC on the control plane.",
		Check: func(ctx context.Context) (bool, error) {
			ok, _, err := helperFirewallLoaded(ctx, r, opts)
			return ok, err
		},
		Apply: func(ctx context.Context) error {
			rules, err := renderHostFirewallRules(ctx, r, opts)
			if err != nil {
				return err
			}
			return submitRootHelperFirewall(ctx, rules)
		},
	}}
}

// helperFirewallLoaded reports whether the helper has loaded the rules grove renders now.
func helperFirewallLoaded(ctx context.Context, r Runner, opts Options) (bool, string, error) {
	rules, err := renderHostFirewallRules(ctx, r, opts)
	if err != nil {
		return false, "", err
	}
	st, err := ReadRootHelperStatus()
	if err != nil {
		return false, RootHelperLabel + " has written no status yet (" + err.Error() + ")", nil
	}
	want := sha256Hex(rules)
	switch {
	case st.RulesSHA256 == want && st.Loaded:
		return true, "anchor " + hostFirewallAnchor + " loaded by " + RootHelperLabel, nil
	case st.RequestSHA256 == want && st.Error != "":
		return false, RootHelperLabel + " refused this grove's rules: " + st.Error, nil
	case !st.Loaded && st.Error != "":
		return false, RootHelperLabel + ": " + st.Error, nil
	default:
		return false, RootHelperLabel + " has not loaded the rules this grove renders yet", nil
	}
}

// submitRootHelperFirewall writes the request into the helper's inbox (a new file renamed into
// place, so the helper never reads half of one) and waits for the helper's answer.
func submitRootHelperFirewall(ctx context.Context, rules string) error {
	if err := ValidateVMFirewallRules(rules); err != nil {
		return fmt.Errorf("this grove's VM firewall rules are outside what the root helper accepts (%v): run `sudo grove root-helper install --user <this user>` from an admin account with this grove", err)
	}
	b, err := json.Marshal(RootHelperRequest{Version: rootHelperVersion, Firewall: &RootHelperFirewallV1{Rules: rules}})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(rootHelperInbox(), ".request-*")
	if err != nil {
		return fmt.Errorf("write a request for %s: %w", RootHelperLabel, err)
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), rootHelperRequest()); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	want := sha256Hex(rules)
	deadline := time.Now().Add(rootHelperWait)
	for {
		if st, err := ReadRootHelperStatus(); err == nil && st.RequestSHA256 == want {
			if st.RulesSHA256 == want && st.Loaded {
				return nil
			}
			if st.Error != "" {
				return fmt.Errorf("%s did not load this grove's VM firewall: %s", RootHelperLabel, st.Error)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s has not answered the VM firewall request in %s (is it loaded? `launchctl print system/%s`)", RootHelperLabel, rootHelperWait, RootHelperLabel)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rootHelperPoll):
		}
	}
}

// --- root's side ---

// RootHelperApply is one pass of the helper (`grove root-helper apply`, run by launchd as root at
// boot, every five minutes and when the inbox changes): keep the last rules that loaded loaded,
// and load a request's rules when they pass the grammar, going back to the last good ones if they
// don't load.
func RootHelperApply(ctx context.Context, r Runner, groveVersion string, out io.Writer) error {
	if rootHelperEUID() != 0 {
		return errors.New("the root helper runs as root, from " + RootHelperLabel)
	}
	cfg, err := readRootHelperConfig()
	if err != nil {
		return err
	}
	retireOldFirewallDaemon(ctx, r, Options{})
	st := RootHelperStatus{Version: 1, Accepts: []int{rootHelperVersion}, GroveVersion: groveVersion, User: cfg.User}
	lastGood, _ := os.ReadFile(rootHelperLastGood())
	current := string(lastGood)

	req, reqErr := readRootHelperRequest(cfg)
	switch {
	case reqErr != nil && !errors.Is(reqErr, os.ErrNotExist):
		st.Error = "request refused: " + reqErr.Error()
	case req != nil && req.Firewall != nil:
		candidate := req.Firewall.Rules
		st.RequestSHA256 = sha256Hex(candidate)
		if err := ValidateVMFirewallRules(candidate); err != nil {
			st.Error = "rules refused: " + err.Error()
		} else if candidate != current {
			if err := loadVMFirewall(ctx, r, candidate); err != nil {
				st.Error = "rules did not load: " + err.Error()
			} else if err := writeRootFile(rootHelperLastGood(), candidate, 0o600); err != nil {
				st.Error = "save the rules: " + err.Error()
				current = candidate
				st.RulesSHA256, st.Loaded = sha256Hex(current), true
			} else {
				current = candidate
				st.RulesSHA256, st.Loaded = sha256Hex(current), true
				fmt.Fprintf(out, "%s root-helper: loaded new VM firewall rules %s\n", time.Now().Format(time.RFC3339), st.RulesSHA256[:12])
			}
		}
	}
	if !st.Loaded && current != "" {
		if err := ValidateVMFirewallRules(current); err != nil {
			st.Error = joinErr(st.Error, "the saved rules are outside the grammar: "+err.Error())
		} else if err := loadVMFirewall(ctx, r, current); err != nil {
			st.Error = joinErr(st.Error, "the saved rules did not load: "+err.Error())
		} else {
			st.RulesSHA256, st.Loaded = sha256Hex(current), true
		}
	}
	if current == "" && st.Error == "" {
		st.Error = "no VM firewall rules yet"
	}
	st.At = time.Now().UTC()
	b, _ := json.MarshalIndent(st, "", "  ")
	if err := writeRootFile(rootHelperStatusPath(), string(b)+"\n", 0o644); err != nil {
		return err
	}
	if st.Error != "" {
		fmt.Fprintf(out, "%s root-helper: %s\n", time.Now().Format(time.RFC3339), st.Error)
	}
	return nil
}

func joinErr(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}

// readRootHelperRequest reads the inbox's request.json: a regular file (never a symlink, a FIFO or
// a device) that the configured user owns, at most rootHelperMaxRequest bytes, strict JSON of a
// version this helper reads. The inbox itself must be a directory that user owns, inside root's.
func readRootHelperRequest(cfg RootHelperConfig) (*RootHelperRequest, error) {
	dirInfo, err := os.Lstat(rootHelperInbox())
	if err != nil {
		return nil, err
	}
	dst, ok := dirInfo.Sys().(*syscall.Stat_t)
	if !ok || !dirInfo.IsDir() || dst.Uid != cfg.UID {
		return nil, fmt.Errorf("the inbox is not %s's directory", cfg.User)
	}
	f, err := os.OpenFile(rootHelperRequest(), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) {
			return nil, errors.New("request.json is a symlink")
		}
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || st.Uid != cfg.UID {
		return nil, fmt.Errorf("request.json is not a regular file of %s's", cfg.User)
	}
	if info.Size() > rootHelperMaxRequest {
		return nil, fmt.Errorf("request.json is over %d bytes", rootHelperMaxRequest)
	}
	b, err := io.ReadAll(io.LimitReader(f, rootHelperMaxRequest+1))
	if err != nil {
		return nil, err
	}
	var req RootHelperRequest
	if err := strictJSON(b, &req); err != nil {
		return nil, fmt.Errorf("request.json: %v", err)
	}
	if req.Version != rootHelperVersion {
		return nil, fmt.Errorf("request version %d; this helper reads version %d (reinstall it: sudo grove root-helper install --user %s)", req.Version, rootHelperVersion, cfg.User)
	}
	return &req, nil
}

// loadVMFirewall loads rules into the anchor with fixed pfctl arguments, the rules on stdin: the
// Mac's pf.conf first only if its com.apple hook is missing, then the anchor, then pf on. It fails
// unless the anchor holds rules and pf is enabled.
func loadVMFirewall(ctx context.Context, r Runner, rules string) error {
	if out, _, _ := r.Run(ctx, "/sbin/pfctl", "-s", "rules"); !strings.Contains(out, `anchor "com.apple/*"`) {
		if _, stderr, err := r.Run(ctx, "/sbin/pfctl", "-q", "-f", "/etc/pf.conf"); err != nil {
			return fmt.Errorf("load /etc/pf.conf: %w %s", err, strings.TrimSpace(stderr))
		}
	}
	if _, stderr, err := r.RunWithStdin(ctx, rules, "/sbin/pfctl", "-q", "-a", hostFirewallAnchor, "-f", "-"); err != nil {
		return fmt.Errorf("pfctl: %w %s", err, strings.TrimSpace(stderr))
	}
	if out, _, _ := r.Run(ctx, "/sbin/pfctl", "-s", "info"); !strings.Contains(out, "Status: Enabled") {
		if _, _, err := r.Run(ctx, "/sbin/pfctl", "-q", "-E"); err != nil {
			_, _, _ = r.Run(ctx, "/sbin/pfctl", "-q", "-e")
		}
		if out, _, _ := r.Run(ctx, "/sbin/pfctl", "-s", "info"); !strings.Contains(out, "Status: Enabled") {
			return errors.New("pf is not enabled")
		}
	}
	out, _, err := r.Run(ctx, "/sbin/pfctl", "-q", "-a", hostFirewallAnchor, "-s", "rules")
	if err != nil || strings.TrimSpace(out) == "" {
		return fmt.Errorf("anchor %s holds no rules after loading", hostFirewallAnchor)
	}
	return nil
}

// retireOldFirewallDaemon removes com.grove.pf-vm, the LaunchDaemon that loaded an older grove's
// rules every five minutes and would put them back over the helper's.
func retireOldFirewallDaemon(ctx context.Context, r Runner, opts Options) bool {
	_, installed := hostFirewallPaths(opts)
	if _, err := os.Lstat(installed); err != nil {
		return false
	}
	_, _, _ = r.Run(ctx, "/bin/launchctl", "bootout", "system/"+hostFirewallLabel)
	waitSystemUnloaded(ctx, r, hostFirewallLabel)
	_ = os.Remove(installed)
	return true
}

// writeRootFile writes path atomically (a temp file beside it, renamed), as this process (root).
func writeRootFile(path, content string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// --- install ---

// RootHelperInstall is `sudo grove root-helper install --user <user>`.
type RootHelperInstall struct {
	User         *user.User
	Exe          string // this process's binary, copied as the helper's
	GroveVersion string
	Out          io.Writer
}

// InstallRootHelper installs the helper for in.User as root: its binary (this process's, after
// checking the worker user can't write it), its directories, config and LaunchDaemon; the VM
// firewall's first rules, rendered here for that user; the retirement of com.grove.pf-vm; and
// the worker Mac's power settings (never sleep, start again after a power cut).
func InstallRootHelper(ctx context.Context, r Runner, in RootHelperInstall) error {
	if rootHelperEUID() != 0 {
		return errors.New("run it with sudo")
	}
	uid64, err := strconv.ParseUint(in.User.Uid, 10, 32)
	if err != nil || uid64 == 0 || !filepath.IsAbs(in.User.HomeDir) {
		return fmt.Errorf("%s is not an ordinary account with a home directory", in.User.Username)
	}
	uid := uint32(uid64)
	exe, err := filepath.EvalSymlinks(in.Exe)
	if err != nil {
		return err
	}
	if who := writableBy(exe, in.User); who != "" {
		return fmt.Errorf("%s can be changed by %s (%s), so it can't become root's: run a grove only an admin can write (e.g. Homebrew's, installed by an admin)", exe, in.User.Username, who)
	}

	// Render the first rules before touching anything, so a control plane that doesn't resolve
	// stops the install with nothing half done.
	opts := Options{Role: RoleWorker, User: in.User.Username, Home: in.User.HomeDir}
	opts.Rootless = !rootDaemonsPresent(ctx, r, opts, workerDaemons(opts))
	rules, err := renderHostFirewallRules(ctx, r, opts)
	if err != nil {
		return err
	}
	if err := ValidateVMFirewallRules(rules); err != nil {
		return fmt.Errorf("this grove's own VM firewall rules fail the helper's grammar (a grove bug): %w", err)
	}

	for _, d := range []struct {
		path string
		mode os.FileMode
		uid  uint32
	}{
		{rootHelperHome, 0o755, 0},
		{filepath.Dir(rootHelperBin()), 0o755, 0},
		{rootHelperState(), 0o700, 0},
		{rootHelperInbox(), 0o700, uid},
	} {
		if err := rootDir(d.path, d.mode, d.uid); err != nil {
			return err
		}
	}
	if err := copyRootBinary(exe, rootHelperBin()); err != nil {
		return err
	}
	cfgJSON, _ := json.MarshalIndent(RootHelperConfig{Version: 1, User: in.User.Username, UID: uid}, "", "  ")
	if err := writeRootFile(rootHelperConfigPath(), string(cfgJSON)+"\n", 0o644); err != nil {
		return err
	}
	if err := writeRootFile(rootHelperLastGood(), rules, 0o600); err != nil {
		return err
	}
	_ = os.Remove(rootHelperRequest()) // a stale request from before must not replace these rules

	if retireOldFirewallDaemon(ctx, r, opts) {
		fmt.Fprintln(in.Out, "Removed "+hostFirewallLabel+": the helper loads the VM firewall now.")
	}
	plist := rootHelperPlistPath()
	if err := writeRootFile(plist, renderRootHelperPlist(), 0o644); err != nil {
		return err
	}
	_, _, _ = r.Run(ctx, "/bin/launchctl", "bootout", "system/"+RootHelperLabel)
	waitSystemUnloaded(ctx, r, RootHelperLabel)
	if _, stderr, err := r.Run(ctx, "/bin/launchctl", "bootstrap", "system", plist); err != nil {
		time.Sleep(2 * time.Second)
		if _, stderr2, err2 := r.Run(ctx, "/bin/launchctl", "bootstrap", "system", plist); err2 != nil {
			return fmt.Errorf("load %s: %w %s %s", RootHelperLabel, err2, strings.TrimSpace(stderr), strings.TrimSpace(stderr2))
		}
	}
	for _, args := range [][]string{{"-a", "disablesleep", "1"}, {"-a", "autorestart", "1"}} {
		if _, stderr, err := r.Run(ctx, "/usr/bin/pmset", args...); err != nil {
			fmt.Fprintf(in.Out, "pmset %s: %v %s\n", strings.Join(args, " "), err, strings.TrimSpace(stderr))
		}
	}

	// launchd runs the first pass at load; wait for its status.
	want := sha256Hex(rules)
	deadline := time.Now().Add(rootHelperWait)
	for {
		st, err := ReadRootHelperStatus()
		if err == nil && st.RulesSHA256 == want && st.Loaded && st.At.After(time.Now().Add(-time.Minute)) {
			fmt.Fprintf(in.Out, "VM firewall loaded by %s for %s.\n", RootHelperLabel, in.User.Username)
			return nil
		}
		if time.Now().After(deadline) {
			detail := "no status"
			if err == nil {
				detail = st.Error
			}
			return fmt.Errorf("%s is installed but has not loaded the VM firewall (%s); see %s", RootHelperLabel, detail, rootHelperLog)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rootHelperPoll):
		}
	}
}

func rootHelperPlistPath() string {
	return filepath.Join(Options{}.launchDaemonsDir(), RootHelperLabel+".plist")
}

// renderRootHelperPlist is the helper's LaunchDaemon: root's (no UserName), at boot, every five
// minutes (pf loses the anchor when /etc/pf.conf is reloaded) and when the inbox changes.
func renderRootHelperPlist() string {
	x := html.EscapeString
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + RootHelperLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + x(rootHelperBin()) + `</string>
		<string>root-helper</string>
		<string>apply</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>/usr/bin:/bin:/usr/sbin:/sbin</string>
	</dict>
	<key>WorkingDirectory</key>
	<string>/</string>
	<key>RunAtLoad</key>
	<true/>
	<key>StartInterval</key>
	<integer>300</integer>
	<key>WatchPaths</key>
	<array>
		<string>` + x(rootHelperInbox()) + `</string>
	</array>
	<key>StandardOutPath</key>
	<string>` + rootHelperLog + `</string>
	<key>StandardErrorPath</key>
	<string>` + rootHelperLog + `</string>
</dict>
</plist>
`
}

// rootDir makes path a directory (never a symlink) owned by uid:wheel with mode.
func rootDir(path string, mode os.FileMode, uid uint32) error {
	if info, err := os.Lstat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if err := os.Chown(path, int(uid), 0); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// copyRootBinary copies src to dst as root's 0755, checking the copy's SHA-256 against the bytes
// it read, so what was checked is what is installed.
func copyRootBinary(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := writeRootFile(dst, string(b), 0o755); err != nil {
		return err
	}
	if err := os.Chown(dst, 0, 0); err != nil {
		return err
	}
	got, err := fileSHA256(dst)
	if err != nil {
		return err
	}
	if got != sha256Hex(string(b)) {
		_ = os.Remove(dst)
		return fmt.Errorf("%s changed while it was copied", src)
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writableBy names how u could change path or a directory above it ("" if u can't): owning it, a
// group of u's with write access, or write access for everyone.
func writableBy(path string, u *user.User) string {
	groups, _ := u.GroupIds()
	inGroup := map[string]bool{}
	for _, g := range groups {
		inGroup[g] = true
	}
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return p + " can't be read: " + err.Error()
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return p + " has no owner"
		}
		perm := info.Mode().Perm()
		sticky := info.Mode()&os.ModeSticky != 0 && info.IsDir()
		switch {
		case strconv.FormatUint(uint64(st.Uid), 10) == u.Uid:
			return "it owns " + p
		case perm&0o020 != 0 && inGroup[strconv.FormatUint(uint64(st.Gid), 10)] && !sticky:
			return "its group can write " + p
		case perm&0o002 != 0 && !sticky:
			return "everyone can write " + p
		}
		if p == "/" || p == filepath.Dir(p) {
			return ""
		}
	}
}
