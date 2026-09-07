package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/apiclient"
	"github.com/gm2211/grove/internal/config"
	"github.com/gm2211/grove/internal/dispatch"
)

func init() {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs <id>",
		Short: "Stream a job's combined stdout+stderr logs.",
		Long: `Stream a job's combined stdout+stderr logs.

Without --follow this is a one-shot snapshot of whatever's been captured so far (nothing to wait
for — a still-pending job just prints nothing). With --follow, it blocks until the job reaches a
terminal status, streaming output as it arrives, then exits with the job's own exit code (124 for
a timed-out job; a generic non-zero code for a lost/failed job with no exit code of its own) and
prints a one-line "job <id> failed (exit N) on <node>" summary to stderr for anything but success.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newAPIClient()
			if err != nil {
				return err
			}
			return streamLogsToStdout(cmd, client, args[0], follow)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "block until the job ends, streaming logs and exiting with its status")
	Root.AddCommand(cmd)
}

// newAPIClient builds an apiclient.Client from the loaded grove config.
func newAPIClient() (*apiclient.Client, error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, err
	}
	if cfg.Server.URL == "" {
		return nil, fmt.Errorf("server.url is not set in the grove config; run `grove config init` or set GROVE_CONFIG")
	}
	return apiclient.New(cfg.Server.URL, cfg.Server.Token), nil
}

// ExitError signals that the RunE that returned it has already printed everything the user needs
// to see (e.g. streamLogsToStdout's one-line job-failure summary) and only wants the process to
// exit with Code — main.go special-cases this to skip its usual "grove: <err>" prefix, which would
// otherwise print a redundant (and, since Error() is empty, blank-looking) second line.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "" }

// followReconnectDelay is how long streamLogsToStdout waits before reopening the NDJSON stream
// after it ends but the job isn't terminal yet (a dropped connection, or the job briefly having no
// allocation) — mirrors the UI's own reconnect delay in ui/src/api/client.ts.
const followReconnectDelay = 1 * time.Second

// streamLogsToStdout streams job id's logs to cmd's stdout/stderr via the NDJSON log endpoint
// (stdout-tagged lines to stdout, stderr-tagged lines to stderr), resuming with sinceOffset on any
// disconnect — the same scheme ui/src/api/client.ts's streamJobLogs already uses successfully.
//
// follow=false does a single fetch and returns once that stream ends (a snapshot, matching the
// pre-existing one-shot `grove logs <id>` behavior) — it does not wait for the job to finish.
//
// follow=true instead keeps reconnecting until GET /jobs/{id} reports the job has reached a
// terminal status, then returns nil for a successful job or an *ExitError carrying the job's exit
// code otherwise (having already printed a "job <id> failed (exit N) on <node>" summary to
// cmd.ErrOrStderr()).
func streamLogsToStdout(cmd *cobra.Command, client *apiclient.Client, id string, follow bool) error {
	ctx := cmd.Context()
	stdout := cmd.OutOrStdout()
	stderr := cmd.ErrOrStderr()

	var offset int64
	for {
		body, _, err := client.JobLogsNDJSON(ctx, id, follow, offset)
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}
		offset, err = copyNDJSONLines(body, stdout, stderr, offset)
		body.Close()
		if err != nil {
			return fmt.Errorf("logs: %w", err)
		}

		if !follow {
			return nil
		}

		job, err := client.GetJob(ctx, id)
		if err != nil {
			return fmt.Errorf("check job %s status: %w", id, err)
		}
		if isTerminalStatus(job.Status) {
			return exitForJob(job, stderr)
		}

		// The stream ended (server-side follow window closed, connection dropped, or the job
		// simply had nothing to say yet) but the job is still going — reconnect and keep waiting.
		select {
		case <-time.After(followReconnectDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// copyNDJSONLines decodes NDJSON log records from r (see apiclient.LogLine), writing each line's
// text (plus a trailing newline) to stdout or stderr per its Stream tag, and returns the offset
// just past the last line written — the sinceOffset a subsequent reconnect should pass to avoid
// re-printing anything already seen. startOffset seeds that running total for a fresh connection
// that already skipped everything before it server-side.
func copyNDJSONLines(r io.Reader, stdout, stderr io.Writer, startOffset int64) (int64, error) {
	offset := startOffset
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		raw := sc.Bytes()
		if len(raw) == 0 {
			continue
		}
		var ln apiclient.LogLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			// Skip lines that fail to parse rather than aborting the whole stream over one bad
			// record — matches ui/src/api/client.ts's streamJobLogs.
			continue
		}
		next := ln.Offset + int64(len(ln.Line)) + 1
		if next > offset {
			offset = next
		}
		w := stdout
		if ln.Stream == "stderr" {
			w = stderr
		}
		fmt.Fprintln(w, ln.Line)
	}
	if err := sc.Err(); err != nil && err != io.EOF {
		return offset, err
	}
	return offset, nil
}

// isTerminalStatus reports whether status is one grove considers final — the job will never
// produce more log output or change status again.
func isTerminalStatus(status dispatch.Status) bool {
	switch status {
	case dispatch.StatusSuccess, dispatch.StatusFailed, dispatch.StatusCanceled, dispatch.StatusLost:
		return true
	default:
		return false
	}
}

// exitForJob returns nil for a successfully-completed job. For anything else it prints a one-line
// "job <id> failed (exit N) on <node>" summary to stderr and returns an *ExitError carrying the
// process exit code that should result: the job's own ExitCode when it has one, 124 for a job
// run.sh's own timeout wrapper killed (see Job.TimedOut), or a generic 1 for a lost/canceled job
// with neither (e.g. it never got as far as producing an exit code at all).
func exitForJob(job *dispatch.Job, stderr io.Writer) error {
	if job.Status == dispatch.StatusSuccess {
		return nil
	}
	code := 1
	switch {
	case job.ExitCode != nil:
		code = *job.ExitCode
	case job.TimedOut:
		code = 124
	}
	if code == 0 {
		code = 1
	}
	node := job.Node
	if node == "" {
		node = "unknown"
	}
	fmt.Fprintf(stderr, "job %s failed (exit %d) on %s\n", job.ID, code, node)
	return &ExitError{Code: code}
}
