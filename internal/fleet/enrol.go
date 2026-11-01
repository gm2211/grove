package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/nomad"
	"github.com/gm2211/grove/internal/orchard"
	"github.com/gm2211/grove/internal/pki"
)

// NodeCertIssuer signs a VM's own Nomad client certificate. *pki.CA implements it.
type NodeCertIssuer interface {
	IssueNode(vmName string, now time.Time, validity time.Duration) (pki.NodeCert, error)
}

// NodeLister lists the Nomad nodes that have registered. The concrete nomad client implements it
// (see nomad.NodePurger).
type NodeLister interface {
	ListNodeSummaries(ctx context.Context) ([]nomad.NodeSummary, error)
}

const (
	// nodeCertRenewBefore is how close to expiry Enrol renews a VM's certificate: halfway through
	// pki.NodeCertValidity, so a control plane that is down for a while doesn't lose its fleet.
	nodeCertRenewBefore = 12 * time.Hour
	// introTokenTTL bounds a VM's introduction token. Nomad caps it at 30 minutes anyway; a VM
	// that hasn't registered by then is enrolled again, with a fresh token.
	introTokenTTL = 30 * time.Minute
	// enrolExecTimeout bounds one enrolment exec, so one VM that doesn't answer can't hold up the
	// rest of the fleet for long.
	enrolExecTimeout = time.Minute
)

// EnrolError is one VM's failed enrolment. Its text is "enrol <vm>: <cause>"; the pool lets
// PoolHealth tell which pool cannot get nodes.
type EnrolError struct {
	VM   string
	Pool string
	Err  error
}

func (e *EnrolError) Error() string { return "enrol " + e.VM + ": " + e.Err.Error() }
func (e *EnrolError) Unwrap() error { return e.Err }

// EnrolState is what Enrol remembers between ticks, in process memory only: after a restart every
// VM is simply enrolled again, which renews its certificate.
type EnrolState struct {
	vms   map[string]enrolled
	pools map[string]bool
}

type enrolled struct {
	uid      string
	at       time.Time
	notAfter time.Time
}

// Enrol gives every running shared-pool VM its Nomad identity, after boot and over Orchard exec,
// so it is never in the VM's spec, where anything that can read Orchard's VM list would see it:
//
//   - its own client certificate and key (pki.CA.IssueNode, a day long, its name inside), renewed
//     halfway through;
//   - when its Nomad client isn't registered yet, an introduction token bound to its name and its
//     pool's node pool, so it can join that pool and no other, as itself and no one else.
//
// The VM's startup script holds Nomad back until the first enrolment lands
// (nomadEnrolWaitScript). A no-op without Options.NodeTLS. Errors are per VM: one VM that fails
// doesn't stop the others, and the next tick tries it again.
func (r *Reconciler) Enrol(ctx context.Context, state *EnrolState) error {
	o := r.Options
	if o.NodeTLS == nil {
		return nil
	}
	if o.NodeCerts == nil || o.NodeIntros == nil || o.Nodes == nil || o.NodePools == nil {
		return errors.New("enrolment needs a node certificate issuer, an introduction token issuer, a node lister and node pools")
	}
	if state.vms == nil {
		state.vms = map[string]enrolled{}
		state.pools = map[string]bool{}
	}

	vms, err := r.Client.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("list vms: %w", err)
	}
	nodes, err := o.Nodes.ListNodeSummaries(ctx)
	if err != nil {
		return err
	}
	// A down node can be a previous VM of the same name, which the new one doesn't share an
	// identity with, and so can one the roll call took out of service (ineligible): only a live,
	// eligible node counts as this VM being registered. A VM whose node is draining still is, for
	// renewing its certificate, but needs no new token.
	registered := map[string]bool{}
	present := map[string]bool{}
	for _, n := range nodes {
		if n.Status == "down" {
			continue
		}
		present[n.Name] = true
		if n.Eligible {
			registered[n.Name] = true
		}
	}

	now := o.now()
	live := map[string]bool{}
	var errs []error
	for _, vm := range vms {
		if vm.Status != "running" || !strings.Contains(vm.StartupScript, nomadEnrolWaitMarker) {
			continue
		}
		pool, worker, _, ok := ParseVMName(vm.Name)
		if !ok || !r.wantsEnrolled(vm, pool, worker) {
			continue
		}
		live[vm.Name] = true
		rec, have := state.vms[vm.Name]
		switch {
		case !have || rec.uid != vm.UID:
		case now.After(rec.notAfter.Add(-nodeCertRenewBefore)):
		case !present[vm.Name] && now.Sub(rec.at) > introTokenTTL:
		default:
			continue
		}
		notAfter, err := r.enrolVM(ctx, state, vm, pool, !registered[vm.Name], now)
		if err != nil {
			errs = append(errs, &EnrolError{VM: vm.Name, Pool: pool, Err: err})
			continue
		}
		state.vms[vm.Name] = enrolled{uid: vm.UID, at: now, notAfter: notAfter}
	}
	for name := range state.vms {
		if !live[name] {
			delete(state.vms, name)
		}
	}
	return errors.Join(errs...)
}

// wantsEnrolled reports whether vm is exactly what the reconciler itself would have created for
// that name: a shared pool's VM, on the worker its name pins it to, not drifted from its spec. Any
// worker's Orchard credential can create VMs, so a VM that merely looks like one of grove's, by
// its name or its startup script, gets no Nomad identity.
func (r *Reconciler) wantsEnrolled(vm orchard.VM, poolName, worker string) bool {
	if r.Spec == nil || hostOf(vm, worker) != worker {
		return false
	}
	for _, p := range r.Spec.Pools {
		if p.Name == poolName {
			return !p.Isolated() && !specDrifted(r.vmSpec(p, worker, vm.Name), vm)
		}
	}
	return false
}

func (r *Reconciler) enrolVM(ctx context.Context, state *EnrolState, vm orchard.VM, pool string, withToken bool, now time.Time) (time.Time, error) {
	o := r.Options
	cert, err := o.NodeCerts.IssueNode(vm.Name, now, pki.NodeCertValidity)
	if err != nil {
		return time.Time{}, err
	}
	token := ""
	if withToken {
		// Strict client introduction refuses a pool that doesn't exist yet.
		if !state.pools[pool] {
			if err := o.NodePools.EnsureNodePool(ctx, pool, "grove fleet pool "+pool); err != nil {
				return time.Time{}, err
			}
			state.pools[pool] = true
		}
		if token, err = o.NodeIntros.CreateIntroToken(ctx, vm.Name, pool, introTokenTTL); err != nil {
			return time.Time{}, err
		}
	}

	ctx, cancel := context.WithTimeout(ctx, enrolExecTimeout)
	defer cancel()
	payload := enrolPayload(token, cert)
	session, err := r.Client.Exec(ctx, vm.Name, []string{"sudo", "-n", "/bin/sh", "-c", shQuote(enrolGuestScript)},
		orchard.ExecOptions{Stdin: strings.NewReader(payload), Wait: enrolExecTimeout / 2})
	if err != nil {
		return time.Time{}, err
	}
	defer session.Close()
	var out bytes.Buffer
	if _, err := io.Copy(&out, io.LimitReader(session.Output(), 64<<10)); err != nil {
		return time.Time{}, err
	}
	code, err := session.Wait(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if code != 0 {
		return time.Time{}, fmt.Errorf("enrolment exited %d: %s", code, strings.TrimSpace(out.String()))
	}
	return cert.NotAfter, nil
}

// enrolPayloadHeader starts what an enrolment exec reads on stdin: then the introduction token
// line (empty for a renewal), then the certificate and key PEM.
const enrolPayloadHeader = "grove-enrol/1"

func enrolPayload(token string, cert pki.NodeCert) string {
	return enrolPayloadHeader + "\n" + token + "\n" + cert.Cert + cert.Key
}

// enrolGuestScript runs as root in the VM (sudo -n, which logs its argv: there is nothing secret
// in it) and reads everything secret from stdin. It writes the certificate and key into the TLS
// directory nomadTLSScript made, the introduction token where the Nomad client looks for it on
// first registration, and the enrolled marker last, which lets the startup script restart Nomad.
// On a renewal, the marker is there already: Nomad reloads its TLS files on SIGHUP and reconnects
// with the new certificate.
var enrolGuestScript = `set -eu
umask 077
if [ "$(uname -s)" = "Darwin" ]; then
  grove_nomad_tls=/usr/local/etc/nomad.d/tls
else
  grove_nomad_tls=/etc/nomad.d/tls
fi
grove_intro=/opt/nomad/data/client/intro_token.jwt
IFS= read -r grove_header
if [ "$grove_header" != "` + enrolPayloadHeader + `" ]; then
  echo "grove: unknown enrolment payload" >&2
  exit 2
fi
IFS= read -r grove_token
mkdir -p "$grove_nomad_tls"
chmod 0700 "$grove_nomad_tls"
cat >"$grove_nomad_tls/.enrol"
sed -n '/^-----BEGIN CERTIFICATE-----$/,/^-----END CERTIFICATE-----$/p' "$grove_nomad_tls/.enrol" >"$grove_nomad_tls/.node.pem"
sed -n '/^-----BEGIN EC PRIVATE KEY-----$/,/^-----END EC PRIVATE KEY-----$/p' "$grove_nomad_tls/.enrol" >"$grove_nomad_tls/.node-key.pem"
rm -f "$grove_nomad_tls/.enrol"
if [ ! -s "$grove_nomad_tls/.node.pem" ] || [ ! -s "$grove_nomad_tls/.node-key.pem" ]; then
  rm -f "$grove_nomad_tls/.node.pem" "$grove_nomad_tls/.node-key.pem"
  echo "grove: enrolment payload has no certificate and key" >&2
  exit 2
fi
if [ -n "$grove_token" ]; then
  mkdir -p "$(dirname "$grove_intro")"
  printf '%s' "$grove_token" >"$grove_intro.new"
  mv -f "$grove_intro.new" "$grove_intro"
fi
mv -f "$grove_nomad_tls/.node-key.pem" "$grove_nomad_tls/node-key.pem"
mv -f "$grove_nomad_tls/.node.pem" "$grove_nomad_tls/node.pem"
if [ -f "$grove_nomad_tls/enrolled" ]; then
  pkill -HUP -x nomad || true
fi
touch "$grove_nomad_tls/enrolled"
echo "grove: enrolled"
`
