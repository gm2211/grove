package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"time"

	"github.com/gm2211/grove/internal/broker"
)

// ErrJobEnded is returned by UpdateCredentials for a job that has already ended.
var ErrJobEnded = errors.New("dispatch: job has ended")

// validateCredentials checks req.Credentials: known kinds, valid and distinct variable names that
// clash with no env key or named secret, and tokens that can go in a header.
func validateCredentials(req JobRequest) error {
	if len(req.Credentials) > MaxCredentials {
		return fmt.Errorf("dispatch: at most %d credentials per job", MaxCredentials)
	}
	seen := make(map[string]bool, len(req.Credentials))
	for _, c := range req.Credentials {
		if !c.Kind.Valid() {
			return fmt.Errorf("dispatch: credential %q has unknown kind %q", c.Env, c.Kind)
		}
		if !envKeyPattern.MatchString(c.Env) {
			return fmt.Errorf("dispatch: credential env %q is not a valid shell variable name", c.Env)
		}
		if seen[c.Env] {
			return fmt.Errorf("dispatch: credential %q is named twice", c.Env)
		}
		seen[c.Env] = true
		if _, clash := req.Env[c.Env]; clash || slices.Contains(req.Secrets, c.Env) {
			return fmt.Errorf("dispatch: %q is both a credential and an env key or named secret", c.Env)
		}
		if err := broker.CheckToken(c.Token); err != nil {
			return fmt.Errorf("dispatch: credential %q: %w", c.Env, err)
		}
	}
	return nil
}

// mintPlaceholders returns a fresh placeholder for each credential, by Env.
func mintPlaceholders(creds []Credential) (map[string]string, error) {
	if len(creds) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(creds))
	for _, c := range creds {
		ph, err := broker.NewPlaceholder()
		if err != nil {
			return nil, fmt.Errorf("dispatch: mint placeholder: %w", err)
		}
		out[c.Env] = ph
	}
	return out, nil
}

// credentialEntries pairs each credential's token with its placeholder, for the registry.
func credentialEntries(id string, creds []Credential, placeholders map[string]string) []broker.Entry {
	out := make([]broker.Entry, 0, len(creds))
	for _, c := range creds {
		out = append(out, broker.Entry{Placeholder: placeholders[c.Env], Job: id, Kind: c.Kind, Token: c.Token})
	}
	return out
}

// withPlaceholders returns env plus each placeholder under its credential's name, leaving env
// itself untouched.
func withPlaceholders(env, placeholders map[string]string) map[string]string {
	if len(placeholders) == 0 {
		return env
	}
	out := make(map[string]string, len(env)+len(placeholders))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range placeholders {
		out[k] = v
	}
	return out
}

// withRedactedCredentials returns req with its credentials' tokens dropped, for the job record.
func withRedactedCredentials(req JobRequest) JobRequest {
	if len(req.Credentials) == 0 {
		return req
	}
	creds := make([]Credential, len(req.Credentials))
	for i, c := range req.Credentials {
		creds[i] = Credential{Kind: c.Kind, Env: c.Env}
	}
	req.Credentials = creds
	return req
}

func (s *service) UpdateCredentials(ctx context.Context, id string, creds []Credential) error {
	rec, ok := s.store.get(id)
	if !ok {
		return ErrNotFound
	}
	job, err := s.reconcile(ctx, rec)
	if err != nil {
		return err
	}
	if isTerminal(job.Status) {
		s.creds.RemoveJob(id)
		return ErrJobEnded
	}
	if len(creds) == 0 {
		return fmt.Errorf("dispatch: no credentials to update")
	}
	kinds := make(map[string]broker.Kind, len(rec.Request.Credentials))
	for _, c := range rec.Request.Credentials {
		kinds[c.Env] = c.Kind
	}
	byEnv := map[string]broker.Entry{}
	for _, e := range s.creds.Job(id) {
		for env, ph := range rec.Placeholders {
			if ph == e.Placeholder {
				byEnv[env] = e
			}
		}
	}
	for _, c := range creds {
		kind, known := kinds[c.Env]
		if !known || rec.Placeholders[c.Env] == "" {
			return fmt.Errorf("dispatch: job %s has no credential %q", id, c.Env)
		}
		if c.Kind != "" && c.Kind != kind {
			return fmt.Errorf("dispatch: credential %q is %s, not %s", c.Env, kind, c.Kind)
		}
		if err := broker.CheckToken(c.Token); err != nil {
			return fmt.Errorf("dispatch: credential %q: %w", c.Env, err)
		}
		byEnv[c.Env] = broker.Entry{Placeholder: rec.Placeholders[c.Env], Job: id, Kind: kind, Token: c.Token}
	}
	envs := make([]string, 0, len(byEnv))
	for env := range byEnv {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	entries := make([]broker.Entry, 0, len(envs))
	for _, env := range envs {
		entries = append(entries, byEnv[env])
	}
	return s.creds.Put(id, entries)
}

func (s *service) BrokerEntries(ctx context.Context, worker string) ([]broker.Entry, error) {
	now := time.Now()
	var out []broker.Entry
	for _, id := range s.creds.Jobs() {
		rec, ok := s.store.get(id)
		if !ok {
			s.creds.RemoveJob(id)
			continue
		}
		if rec.Placement == nil || rec.Placement.WorkerID == "" {
			// A job that is just starting needs its placement now, not once the status cache
			// expires: its broker is asking because the job already sent a placeholder.
			s.mu.Lock()
			delete(s.cache, id)
			s.mu.Unlock()
		}
		job, err := s.reconcile(ctx, rec)
		if err != nil {
			slog.Warn("dispatch: reconcile for broker entries failed", "id", id, "err", err)
			continue
		}
		if isTerminal(job.Status) {
			s.creds.RemoveJob(id)
			continue
		}
		if job.Placement == nil || job.Placement.WorkerID != worker {
			continue
		}
		for _, e := range s.creds.Job(id) {
			e.VM = job.Placement.VMID
			e.Expires = now.Add(broker.Lease)
			out = append(out, e)
		}
	}
	return out, nil
}
