package install

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

const testBootstrapCmd = "nomad acl bootstrap -address=http://cp:4646 -json"

func nomadACLTestSetup(t *testing.T) (Options, string) {
	t.Helper()
	old := nomadACLBootstrapWait
	nomadACLBootstrapWait = 0
	t.Cleanup(func() { nomadACLBootstrapWait = old })
	opts := Options{Role: RoleControlPlane, Home: t.TempDir(), GOOS: "linux"}
	cfgDir := opts.ConfigDir()
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := updateConfig(cfgDir, func(c *config.Config) { c.Nomad.URL = "http://cp:4646" }); err != nil {
		t.Fatal(err)
	}
	return opts, cfgDir
}

func TestNomadACLBootstrap_StoresManagementToken(t *testing.T) {
	opts, cfgDir := nomadACLTestSetup(t)
	r := NewFakeRunner()
	r.Script(testBootstrapCmd, FakeResult{Stdout: `{"AccessorID":"a","SecretID":"mgmt-secret"}`})
	step := nomadACLBootstrapStep(r, opts, false)

	if done, err := step.Check(context.Background()); err != nil || done {
		t.Fatalf("Check before bootstrap = %v, %v; want false", done, err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadOrInitConfig(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Nomad.Token != "mgmt-secret" {
		t.Fatalf("nomad.token = %q", cfg.Nomad.Token)
	}
	for _, c := range r.Calls {
		if strings.Contains(c.String(), "mgmt-secret") {
			t.Errorf("management token reached argv: %s", c.String())
		}
	}
	if done, _ := step.Check(context.Background()); !done {
		t.Error("Check after bootstrap should be satisfied")
	}
}

// A control plane installed while ACLs were off keeps its running Nomad until it is restarted.
func TestNomadACLBootstrap_RestartsNomadWhenACLsStillOff(t *testing.T) {
	opts, cfgDir := nomadACLTestSetup(t)
	r := &scriptedSequence{FakeRunner: NewFakeRunner(), responses: []FakeResult{
		{Stderr: "Error bootstrapping: Unexpected response code: 400 (ACL support disabled)", Err: errors.New("exit status 1")},
		{Stderr: "No cluster leader", Err: errors.New("exit status 1")},
		{Stdout: `{"SecretID":"after-restart"}`},
	}}
	if err := nomadACLBootstrapStep(r, opts, false).Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("systemctl --user restart grove-nomad.service") {
		t.Errorf("Nomad was not restarted; calls: %v", r.Calls)
	}
	cfg, _ := loadOrInitConfig(cfgDir)
	if cfg.Nomad.Token != "after-restart" {
		t.Fatalf("nomad.token = %q", cfg.Nomad.Token)
	}
}

func TestNomadACLBootstrap_AlreadyBootstrappedExplainsFix(t *testing.T) {
	opts, _ := nomadACLTestSetup(t)
	r := NewFakeRunner()
	r.Script(testBootstrapCmd, FakeResult{Stderr: "ACL bootstrap already done (reset index: 7)", Err: errors.New("exit status 1")})
	err := nomadACLBootstrapStep(r, opts, true).Apply(context.Background())
	if err == nil || !strings.Contains(err.Error(), "grove config set nomad.token") {
		t.Fatalf("err = %v, want guidance to set nomad.token", err)
	}
	if n := len(r.Calls); n != 1 {
		t.Errorf("should not retry an already-bootstrapped cluster, made %d calls", n)
	}
}

// scriptedSequence answers successive bootstrap calls from responses, everything else from the
// embedded FakeRunner.
type scriptedSequence struct {
	*FakeRunner
	responses []FakeResult
}

func (s *scriptedSequence) Run(ctx context.Context, name string, args ...string) (string, string, error) {
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	if line == testBootstrapCmd && len(s.responses) > 0 {
		res := s.responses[0]
		s.responses = s.responses[1:]
		s.FakeRunner.Calls = append(s.FakeRunner.Calls, Call{Name: name, Args: args})
		return res.Stdout, res.Stderr, res.Err
	}
	return s.FakeRunner.Run(ctx, name, args...)
}
