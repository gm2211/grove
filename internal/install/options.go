package install

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	Controller string // Orchard controller URL, e.g. https://grove-cp.tailnet.ts.net:6120
	NomadAddr  string // Nomad HTTP API URL
	ServerURL  string // grove server URL (client role, and recorded on control-plane/worker too)
	Token      string // bootstrap / server token, meaning depends on Role

	// Registry/RegistryUser/RegistryToken configure `tart login` for a worker's private image
	// pulls (see docs/IMAGES.md "Pushing to GHCR": ghcr.io packages are private by default, and
	// GitHub has no API to flip that — UI only). Registry defaults to "ghcr.io" via registry()
	// below. RegistryToken is never persisted or logged: it's only ever fed to `tart login
	// --password-stdin` over stdin (see Runner.RunWithStdin), never as a command argument.
	Registry      string
	RegistryUser  string
	RegistryToken string

	Hostname string // test override; defaults to os.Hostname()
	Home     string // test override; defaults to os.UserHomeDir()
	GOOS     string // test override; defaults to runtime.GOOS
	GOARCH   string // test override; defaults to runtime.GOARCH

	// LookPath resolves a binary name to a path like exec.LookPath. Test override.
	LookPath func(name string) (string, error)
	// Exists checks whether a path exists, used by FindTailscale for the hardcoded standalone/App
	// Store bundle paths. Test override, so a test isn't at the mercy of what's actually installed
	// on the machine running it; defaults to a real os.Stat-backed check.
	Exists func(path string) bool
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

// LogDir is where grove-supervised process logs are written.
func (o Options) LogDir() string {
	return filepath.Join(o.homeDir(), "Library", "Logs", "grove")
}

// SystemdUserDir is where Linux --user units are rendered.
func (o Options) SystemdUserDir() string {
	return filepath.Join(o.homeDir(), ".config", "systemd", "user")
}
