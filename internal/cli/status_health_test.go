package cli

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
)

type deniedNomad struct {
	nomad.Client
	err error
}

func (d deniedNomad) ListNodeTokens(context.Context) ([]nomad.NodeToken, error) { return nil, d.err }

type vmsOrchard struct {
	orchard.Client
	vms []orchard.VM
}

func (v vmsOrchard) ListVMs(context.Context) ([]orchard.VM, error) { return v.vms, nil }

func TestPrintNomadPermissions(t *testing.T) {
	var out bytes.Buffer
	printNomadPermissions(context.Background(), &out, deniedNomad{err: fmt.Errorf("nomad: list ACL tokens: %w", nomad.ErrPermissionDenied)})
	if got := out.String(); !strings.Contains(got, "DENIED") || !strings.Contains(got, "set config.yaml nomad.token to a management token") {
		t.Errorf("denied output = %q", got)
	}
	out.Reset()
	printNomadPermissions(context.Background(), &out, deniedNomad{})
	if out.Len() != 0 {
		t.Errorf("a management token prints nothing, got %q", out.String())
	}
}

func TestPrintBootLoops(t *testing.T) {
	var out bytes.Buffer
	printBootLoops(context.Background(), &out, vmsOrchard{vms: []orchard.VM{
		{Name: "linux-a-0", Worker: "a", RestartCount: 9},
		{Name: "linux-b-0", Worker: "b"},
	}})
	got := out.String()
	if !strings.Contains(got, "boot loop: worker a: 1 VM(s) boot-looping (up to 9 restarts") || strings.Contains(got, "worker b") {
		t.Errorf("output = %q", got)
	}
}
