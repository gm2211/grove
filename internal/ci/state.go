package ci

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// state is what the watcher remembers between polls and across restarts: every commit it has
// seen, and the last status it posted for each of that commit's jobs. It holds no credential.
type state struct {
	Version int                `json:"version"`
	Commits map[string]*commit `json:"commits"`
}

// commit is one commit the watcher has acted on. A commit with no .grove.yml is remembered too,
// with no runs, so its file is not fetched again every poll.
type commit struct {
	Host     string    `json:"host"`
	Repo     string    `json:"repo"`
	SHA      string    `json:"sha"`
	Trigger  string    `json:"trigger"`
	SeenAt   time.Time `json:"seenAt"`
	Runs     []*run    `json:"runs,omitempty"`
	Finished bool      `json:"finished,omitempty"`
}

// run is one job of a commit, and what GitHub was last told about it.
type run struct {
	// Context is the commit status name, "grove/<job>", or "grove" for a .grove.yml that could
	// not be read.
	Context string `json:"context"`
	// JobID is the grove job; empty when the job could not be started, in which case Error says
	// why and is what GitHub is told.
	JobID string `json:"jobId,omitempty"`
	Error string `json:"error,omitempty"`
	// Posted is the last status GitHub accepted for this run.
	Posted *CommitStatus `json:"posted,omitempty"`
	// Final is set once a terminal status has been posted; the run is never looked at again.
	Final bool `json:"final,omitempty"`
}

func commitKey(r Repo, sha string) string { return r.Host + "/" + r.FullName + "@" + sha }

func (c *commit) repo() Repo { return Repo{Host: c.Host, FullName: c.Repo} }

// defaultStatePath is ci.json next to grove's job history.
func defaultStatePath() string {
	if base := os.Getenv("XDG_STATE_HOME"); base != "" {
		return filepath.Join(base, "grove", "ci.json")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "grove", "ci.json")
}

func loadState(path string) (*state, error) {
	st := &state{Version: 1, Commits: map[string]*commit{}}
	if path == "" {
		return st, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if st.Commits == nil {
		st.Commits = map[string]*commit{}
	}
	return st, nil
}

// save writes the state through a same-directory rename, so a crash never leaves half a file.
func (st *state) save(path string) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
