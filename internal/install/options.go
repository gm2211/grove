package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/wire"
)

// Role selects which step set BuildPlan assembles.
type Role string

const (
	RoleWorker       Role = "worker"
	RoleControlPlane Role = "control-plane"
	RoleClient       Role = "client"
)

// Options carries every flag/environment input the planners need. All the fields with a comment
// "test override" exist so unit tests can point the planner at a scratch directory / fake PATH
// instead of the real machine.
type Options struct {
	Role       Role
	identity   workerIdentity // set by resolveWorkerIdentity for the worker plan
	Controller string         // Orchard controller URL, e.g. https://grove-cp.tailnet.ts.net:6120
	NomadAddr  string         // Nomad HTTP API URL
	ServerURL  string         // grove server URL (client role, and recorded on control-plane/worker too)
	Token      string         // bootstrap / server token, meaning depends on Role
	// BrokerToken is a worker's broker credential (only the broker scope, named after the
	// worker), from `grove setup`. Install keeps it in a 0600 file under ~/.config/grove/credentials
	// for `grove broker`; it never appears in arguments, launchd plists or the plan's output.
	BrokerToken string

	// Registry/RegistryUser/RegistryToken configure `tart login` for a worker's private image
	// pulls (see docs/IMAGES.md "Pushing to GHCR": ghcr.io packages are private by default, and
	// GitHub has no API to flip that — UI only). Registry defaults to "ghcr.io" via registry()
	// below. RegistryToken is never persisted or logged: it's only ever fed to `tart login
	// --password-stdin` over stdin (see Runner.RunWithStdin), never as a command argument.
	Registry      string
	RegistryUser  string
	RegistryToken string

	// Softnet adds Softnet to a worker's plan: the VM network filter fleet.yaml pools with
	// network: isolated run on, plus the one-time step that lets it become root. Off by default,
	// so a Mac that never runs an isolated pool doesn't get a root-capable binary.
	Softnet bool

	// Rootless installs the worker with no root at all: per-user LaunchAgents instead of
	// LaunchDaemons, a keep-awake agent instead of pmset, no VM firewall (rootless.go). Set by
	// --rootless, by config.yaml's worker.rootless, or by AutoRootless when the user has no sudo.
	Rootless bool
	// NoAutoRootless keeps the root-based install even for a user who isn't an admin
	// (--rootless=false), and ends a rootless install recorded in config.yaml.
	NoAutoRootless bool
	// Firewall keeps the VM firewall (host-firewall, one root step for an admin) in a rootless
	// install, which otherwise skips it. The worker's own jobs stay LaunchAgents: pf is
	// independent of how the worker launches. Once the firewall is installed, later rootless runs
	// keep it up to date without the flag.
	Firewall bool

	// Hostname names this machine to grove: the worker's Orchard name (`grove install --name`).
	// Empty means the worker name already installed on this Mac, else os.Hostname().
	Hostname string
	// Labels are the worker's Orchard labels ("k=v,k=v"; `grove install --labels`). Empty means the
	// labels already installed, else host=<name>,arch=arm64.
	Labels string
	// Force lets a rename of a worker proceed on a host that cannot also rewrite fleet.yaml and
	// delete the old Orchard worker record (a worker host that is not the control plane).
	Force bool
	// User is the account a worker's LaunchDaemons run as (their UserName). Test override;
	// defaults to the user running the install, who must not be root.
	User string
	// GroveBin is the grove binary the broker's LaunchDaemon runs. Empty means grove on PATH, else
	// this binary. `grove setup --user` sets it on both sides (the admin's grove, which pins the
	// plist, and the worker user's, which stages it) so they render the same plist.
	GroveBin string
	Home     string // test override; defaults to os.UserHomeDir()
	// GroveVersion is this grove binary's release version ("0.1.21"; empty or "dev" for a local
	// build). The Orchard binary is downloaded from that release's assets before any source build.
	GroveVersion string
	GOOS         string // test override; defaults to runtime.GOOS
	GOARCH       string // test override; defaults to runtime.GOARCH
	// LaunchDaemons is where system LaunchDaemons live. Test override; defaults to
	// /Library/LaunchDaemons.
	LaunchDaemons string

	// UsersDir is where other accounts' home directories live, searched for superseded worker
	// stacks. Test override; defaults to /Users.
	UsersDir string
	// LookupUID resolves another account's uid. Test override; defaults to os/user.
	LookupUID func(name string) (int, bool)
	// Now stamps the backup directory. Test override.
	Now func() time.Time
	// RetireWorker deletes an Orchard worker record on the controller. Test override; defaults to
	// the authenticated Orchard client.
	RetireWorker func(ctx context.Context, ep config.Endpoint, name string) error
	// OrchardClient builds the Orchard client doctor uses. Test override; defaults to
	// wire.NewOrchardClient.
	OrchardClient func(config.Endpoint) (orchard.Client, error)

	// LookPath resolves a binary name to a path like exec.LookPath. Test override.
	LookPath func(name string) (string, error)
	// Exists checks whether a path exists, used by FindTailscale for the hardcoded standalone/App
	// Store bundle paths. Test override, so a test isn't at the mercy of what's actually installed
	// on the machine running it; defaults to a real os.Stat-backed check.
	Exists func(path string) bool
	// NomadClient builds the Nomad API client the control-plane plan uses once Nomad is up (to
	// mint the sandbox-host client's introduction token). Test override; defaults to
	// wire.NewNomadClient.
	NomadClient func(config.Endpoint) (nomad.Client, error)
}

func (o Options) nomadClient(ep config.Endpoint) (nomad.Client, error) {
	if o.NomadClient != nil {
		return o.NomadClient(ep)
	}
	return wire.NewNomadClient(ep)
}

func (o Options) orchardClient(ep config.Endpoint) (orchard.Client, error) {
	if o.OrchardClient != nil {
		return o.OrchardClient(ep)
	}
	return wire.NewOrchardClient(ep)
}

// workerLabels are the labels the worker advertises: --labels, else host=<name>,arch=arm64.
func (o Options) workerLabels() string {
	if o.Labels != "" {
		return o.Labels
	}
	return defaultWorkerLabels(o.hostname())
}

func (o Options) hostname() string {
	if o.Hostname != "" {
		return o.Hostname
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "grove-host"
}

// workerUser is the account the worker's LaunchDaemons run as. A daemon without a UserName runs as
// root, and the worker's wrapper, Orchard and the jobs it starts are this user's files, so root is
// refused here, once, rather than checked at each place a plist is rendered.
func (o Options) workerUser() (string, error) {
	name := o.User
	if name == "" {
		u, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("find the user running this install: %w", err)
		}
		name = u.Username
		if u.Uid == "0" {
			name = "root"
		}
	}
	if name == "root" {
		return "", errors.New("the worker must run as an ordinary user, not root: run `grove install --role worker` (or `grove setup`) as the account the worker should run as, such as `su - argos`, and let an admin run its root steps (it prints one block for them)")
	}
	return name, nil
}

// workerUID is the worker user's uid: User's when it names an account on this machine (the admin's
// `grove setup --user` renders the root script for another user), else this process's.
func (o Options) workerUID() int {
	if o.User != "" {
		if u, err := user.Lookup(o.User); err == nil {
			if uid, err := strconv.Atoi(u.Uid); err == nil {
				return uid
			}
		}
	}
	return os.Getuid()
}

func (o Options) homeDir() string {
	if o.Home != "" {
		return o.Home
	}
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/tmp"
}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

func (o Options) goarch() string {
	if o.GOARCH != "" {
		return o.GOARCH
	}
	return runtime.GOARCH
}

func (o Options) lookPath() func(string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath
	}
	return exec.LookPath
}

func (o Options) exists() func(string) bool {
	return o.Exists // nil is fine: FindTailscale substitutes the real check itself
}

// registry is o.Registry, defaulting to "ghcr.io" — where grove's own worker images are published
// (see docs/IMAGES.md).
func (o Options) registry() string {
	if o.Registry != "" {
		return o.Registry
	}
	return "ghcr.io"
}

// ConfigDir is ~/.config/grove (honours GROVE_CONFIG's directory if set, then Options.Home).
func (o Options) ConfigDir() string {
	if p := os.Getenv("GROVE_CONFIG"); p != "" {
		return filepath.Dir(p)
	}
	return filepath.Join(o.homeDir(), ".config", "grove")
}

// LaunchAgentsDir is where macOS per-user launchd agents are rendered.
func (o Options) LaunchAgentsDir() string {
	return filepath.Join(o.homeDir(), "Library", "LaunchAgents")
}

// launchDaemonsDir is where system (root) LaunchDaemons are installed.
func (o Options) launchDaemonsDir() string {
	if o.LaunchDaemons != "" {
		return o.LaunchDaemons
	}
	return "/Library/LaunchDaemons"
}

// LogDir is where grove-supervised process logs are written.
func (o Options) LogDir() string {
	return filepath.Join(o.homeDir(), "Library", "Logs", "grove")
}

// SystemdUserDir is where Linux --user units are rendered.
func (o Options) SystemdUserDir() string {
	return filepath.Join(o.homeDir(), ".config", "systemd", "user")
}

// userIsAdmin reports whether the user running this process is in macOS's admin group, i.e. can
// use sudo.
func userIsAdmin(ctx context.Context, r Runner) bool {
	stdout, _, err := r.Run(ctx, "/usr/bin/id", "-Gn")
	if err != nil {
		return false
	}
	for _, g := range strings.Fields(stdout) {
		if g == "admin" {
			return true
		}
	}
	return false
}
