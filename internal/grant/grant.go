// Package grant is the one allow-list syntax grove uses to say which principals may have an
// operator-set value used on their behalf: a named job secret (internal/secrets) or the armed
// private-repo credential (internal/gitauth).
//
// An entry is a principal id (a device credential's id, or a Tailscale identity's
// "tailscale:<stable id>") or a Tailscale node tag ("tag:argos"). An operator is always allowed;
// an empty list allows operators only.
package grant

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Caller is who is asking: the authenticated principal behind a job submission.
type Caller struct {
	ID       string
	Tags     []string
	Operator bool
}

// Granted reports whether allow names c, by id or by one of its tags. It does not look at
// c.Operator: callers decide what an operator may do.
func Granted(allow []string, c Caller) bool {
	for _, entry := range allow {
		if strings.HasPrefix(entry, "tag:") {
			for _, tag := range c.Tags {
				if tag == entry {
					return true
				}
			}
			continue
		}
		if entry == c.ID && c.ID != "" {
			return true
		}
	}
	return false
}

// Normalize trims, de-duplicates and sorts an allow list, refusing an entry that is not one
// principal id or tag:<name>.
func Normalize(allow []string) ([]string, error) {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(allow))
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.ContainsAny(entry, " \t\r\n,") {
			return nil, fmt.Errorf("allow entry %q must be one principal id or tag:<name>", entry)
		}
		if entry == "tag:" {
			return nil, errors.New("allow entry \"tag:\" names no tag")
		}
		if _, dup := seen[entry]; dup {
			continue
		}
		seen[entry] = struct{}{}
		out = append(out, entry)
	}
	sort.Strings(out)
	return out, nil
}
