package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

// NodeTokenPolicy is the Nomad ACL policy every per-VM node token carries: enough for a VM's
// shutdown script to find its own node ID (`-self` reads /v1/agent/self, agent:read), drain it
// (node:write) and watch the drain finish (node:read), and nothing else. It grants no namespace
// capability, so a node token can't read job specs, dispatch payloads, logs or other allocations'
// files.
const NodeTokenPolicy = "grove-node"

// NodeTokenPrefix starts the Name of every token grove mints for a VM, so ListNodeTokens can tell
// them apart from operator-made tokens and never revokes anything else.
const NodeTokenPrefix = "grove-vm/"

const nodeTokenPolicyRules = `# Managed by grove: lets a VM drain its own Nomad client on recycle.
node {
  policy = "write"
}

agent {
  policy = "read"
}
`

// NodeToken is a per-VM Nomad ACL token. SecretID is only set on the value CreateNodeToken returns;
// listed tokens carry the AccessorID, Name and CreateTime alone.
type NodeToken struct {
	AccessorID string
	SecretID   string
	Name       string
	CreateTime time.Time
}

// NodeTokens mints and revokes the per-VM Nomad ACL tokens the fleet reconciler hands to each VM's
// shutdown script. It needs a management token (config.yaml nomad.token). The concrete Client
// returned by New implements it; see AsNodeTokens.
type NodeTokens interface {
	// CreateNodeToken makes sure the NodeTokenPolicy policy exists, then mints a client token
	// named NodeTokenPrefix+vmName carrying only that policy.
	CreateNodeToken(ctx context.Context, vmName string) (NodeToken, error)
	// ListNodeTokens lists the tokens whose Name starts with NodeTokenPrefix.
	ListNodeTokens(ctx context.Context) ([]NodeToken, error)
	// RevokeNodeToken deletes one token by accessor. An already-deleted token is not an error.
	RevokeNodeToken(ctx context.Context, accessorID string) error
}

// AsNodeTokens returns c's NodeTokens implementation, if it has one.
func AsNodeTokens(c Client) (NodeTokens, bool) {
	t, ok := c.(NodeTokens)
	return t, ok
}

func (c *client) CreateNodeToken(ctx context.Context, vmName string) (NodeToken, error) {
	if vmName == "" {
		return NodeToken{}, errors.New("nomad: node token needs a VM name")
	}
	if _, err := c.raw.ACLPolicies().Upsert(&nomadapi.ACLPolicy{
		Name:        NodeTokenPolicy,
		Description: "grove: a VM drains its own Nomad client on recycle",
		Rules:       nodeTokenPolicyRules,
	}, wOpts(ctx)); err != nil {
		return NodeToken{}, fmt.Errorf("nomad: upsert ACL policy %s: %w", NodeTokenPolicy, err)
	}
	created, _, err := c.raw.ACLTokens().Create(&nomadapi.ACLToken{
		Name:     NodeTokenPrefix + vmName,
		Type:     "client",
		Policies: []string{NodeTokenPolicy},
	}, wOpts(ctx))
	if err != nil {
		return NodeToken{}, fmt.Errorf("nomad: create node token for %s: %w", vmName, err)
	}
	if created == nil || created.AccessorID == "" || created.SecretID == "" {
		return NodeToken{}, fmt.Errorf("nomad: create node token for %s: empty response", vmName)
	}
	return NodeToken{
		AccessorID: created.AccessorID,
		SecretID:   created.SecretID,
		Name:       created.Name,
		CreateTime: created.CreateTime,
	}, nil
}

func (c *client) ListNodeTokens(ctx context.Context) ([]NodeToken, error) {
	stubs, _, err := c.raw.ACLTokens().List(qOpts(ctx))
	if err != nil {
		return nil, fmt.Errorf("nomad: list ACL tokens: %w", err)
	}
	var out []NodeToken
	for _, s := range stubs {
		if s == nil || !strings.HasPrefix(s.Name, NodeTokenPrefix) {
			continue
		}
		out = append(out, NodeToken{AccessorID: s.AccessorID, Name: s.Name, CreateTime: s.CreateTime})
	}
	return out, nil
}

func (c *client) RevokeNodeToken(ctx context.Context, accessorID string) error {
	if _, err := c.raw.ACLTokens().Delete(accessorID, wOpts(ctx)); err != nil {
		if errors.Is(wrapNotFound(err), ErrNotFound) {
			return nil
		}
		return fmt.Errorf("nomad: revoke node token %s: %w", accessorID, err)
	}
	return nil
}
