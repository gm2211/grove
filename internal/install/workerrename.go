package install

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/wire"
)

// A worker whose name or labels change (an explicit --name or --labels that differs from what is
// installed) leaves two things pointing at the old identity: the Orchard worker record, and the
// workerSelector entries in fleet.yaml that matched its labels. On the control plane host the
// install fixes both; anywhere else it cannot, so it refuses (without --force) and prints the exact
// change, rather than orphaning the pools quietly.

// controlPlaneConfig is this user's config when this Mac hosts the control plane: a config.yaml
// that names a fleet.yaml that exists here.
func controlPlaneConfig(opts Options) (*config.Config, bool) {
	cfg, err := config.LoadFrom(configFilePath(opts))
	if err != nil || cfg.Fleet == "" {
		return nil, false
	}
	if _, err := os.Stat(cfg.Fleet); err != nil {
		return nil, false
	}
	return cfg, true
}

// workerDeleter is what removes a worker record from the Orchard controller. It is not part of
// orchard.Client (a worker record is only ever deleted here); the real client implements it.
type workerDeleter interface {
	DeleteWorker(ctx context.Context, name string) error
}

// retireWorkerRecord deletes the Orchard worker record name, if the controller has one.
func (o Options) retireWorkerRecord(ctx context.Context, ep config.Endpoint, name string) error {
	if o.RetireWorker != nil {
		return o.RetireWorker(ctx, ep, name)
	}
	if ep.URL == "" || ep.Token == "" {
		return fmt.Errorf("no Orchard admin credential in config.yaml to delete worker %q with", name)
	}
	c, err := wire.NewOrchardClient(ep)
	if err != nil {
		return err
	}
	workers, err := c.ListWorkers(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, w := range workers {
		if w.Name == name {
			found = true
		}
	}
	if !found {
		return nil
	}
	d, ok := c.(workerDeleter)
	if !ok {
		return fmt.Errorf("this Orchard client cannot delete workers")
	}
	return d.DeleteWorker(ctx, name)
}

// fleetSelectorEdit is one workerSelector value rewritten.
type fleetSelectorEdit struct{ Pool, Key, Old, New string }

// rewriteFleetSelectors points the workerSelector of every pool that matched a worker with oldLabels
// at newLabels. A pool whose selector the old labels did not satisfy is not that worker's, and is
// left alone. Comments and layout survive (the YAML is edited as a node tree).
func rewriteFleetSelectors(data []byte, oldLabels, newLabels string) ([]byte, []fleetSelectorEdit, error) {
	changes := selectorChanges(oldLabels, newLabels)
	if len(changes) == 0 {
		return data, nil, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, err
	}
	if len(doc.Content) == 0 {
		return data, nil, nil
	}
	oldMap := parseLabels(oldLabels)
	var edits []fleetSelectorEdit
	pools := mappingValue(doc.Content[0], "pools")
	if pools == nil || pools.Kind != yaml.SequenceNode {
		return data, nil, nil
	}
	for _, pool := range pools.Content {
		sel := mappingValue(pool, "workerSelector")
		if sel == nil || sel.Kind != yaml.MappingNode || len(sel.Content) == 0 {
			continue
		}
		matched := true
		for i := 0; i+1 < len(sel.Content); i += 2 {
			if oldMap[sel.Content[i].Value] != sel.Content[i+1].Value {
				matched = false
			}
		}
		if !matched {
			continue
		}
		poolName := ""
		if n := mappingValue(pool, "name"); n != nil {
			poolName = n.Value
		}
		for i := 0; i+1 < len(sel.Content); i += 2 {
			k, v := sel.Content[i].Value, sel.Content[i+1]
			for _, c := range changes {
				if c.Key != k || c.Old != v.Value {
					continue
				}
				if c.New == "" {
					return nil, nil, fmt.Errorf("pool %q selects %s=%s, which the worker's new labels (%s) no longer carry; add it with --labels or change the pool's workerSelector", poolName, k, c.Old, newLabels)
				}
				edits = append(edits, fleetSelectorEdit{Pool: poolName, Key: k, Old: c.Old, New: c.New})
				v.Value = c.New
			}
		}
	}
	if len(edits) == 0 {
		return data, nil, nil
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, nil, err
	}
	return buf.Bytes(), edits, nil
}

func mappingValue(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

// renameWorkerStep finishes a rename on the control plane host once the renamed worker's daemon is
// loaded: it points fleet.yaml's selectors at the new labels and deletes the old Orchard worker
// record. The old identities stay in the identity record until both are done, so a failure here, or
// an admin who runs the daemons step later, is retried by the next run.
func renameWorkerStep(r Runner, opts Options, id workerIdentity, ds []daemon, bk *backupSession, out io.Writer) Step {
	pending := id.Pending
	return Step{
		Name: "worker-rename",
		Description: fmt.Sprintf("Point fleet.yaml's workerSelector entries at %q (%s) and delete the old Orchard worker record(s) %s.",
			id.Name, id.Labels, pendingNames(pending)),
		Check: func(context.Context) (bool, error) { return len(pending) == 0, nil },
		Apply: func(ctx context.Context) error {
			if !daemonLoaded(ctx, r, ds[0]) {
				fmt.Fprintf(out, "        the renamed worker is not loaded yet; fleet.yaml and the old worker record %s are updated once it is (re-run `grove install --role worker`)\n", pendingNames(pending))
				return nil
			}
			cfg, ok := controlPlaneConfig(opts)
			if !ok {
				return fmt.Errorf("this Mac no longer has the control plane's fleet.yaml; make the change by hand:\n%s", renameNotes(pending, id))
			}
			data, err := os.ReadFile(cfg.Fleet)
			if err != nil {
				return err
			}
			updated := data
			for _, p := range pending {
				next, edits, err := rewriteFleetSelectors(updated, p.Labels, id.Labels)
				if err != nil {
					return err
				}
				updated = next
				for _, e := range edits {
					fmt.Fprintf(out, "        fleet.yaml: pool %s workerSelector %s: %s -> %s\n", e.Pool, e.Key, e.Old, e.New)
				}
			}
			if !bytes.Equal(updated, data) {
				if err := bk.save(cfg.Fleet); err != nil {
					return err
				}
				if err := os.WriteFile(cfg.Fleet, updated, 0o644); err != nil {
					return err
				}
			}
			for _, p := range pending {
				if p.Name == id.Name {
					continue
				}
				if err := opts.retireWorkerRecord(ctx, cfg.Orchard, p.Name); err != nil {
					return fmt.Errorf("delete the old Orchard worker record %q: %w (the rest is done; re-run to retry)", p.Name, err)
				}
				fmt.Fprintf(out, "        deleted the old Orchard worker record %q\n", p.Name)
			}
			done := id
			done.Pending = nil
			return writeFile(workerIdentityPath(opts), renderWorkerIdentityRecord(done))
		},
	}
}

func pendingNames(p []pendingIdentity) string {
	var n []string
	for _, x := range p {
		n = append(n, fmt.Sprintf("%q", x.Name))
	}
	return strings.Join(n, ", ")
}

func renameNotes(pending []pendingIdentity, id workerIdentity) string {
	var b strings.Builder
	for _, p := range pending {
		b.WriteString(renameFleetNote(workerIdentity{Name: p.Name, Labels: p.Labels}, id))
	}
	return b.String()
}
