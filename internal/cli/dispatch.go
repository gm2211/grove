package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/dispatch"
)

func init() {
	var (
		pool    string
		kind    string
		repo    string
		ref     string
		envs    []string
		timeout time.Duration
		follow  bool
	)
	cmd := &cobra.Command{
		Use:   "dispatch --pool <pool> [flags] -- <script...>",
		Short: "Submit a job to the fleet via the grove server.",
		Long: `Submit a job to the fleet via the grove server.

The script (everything after "--") runs with bash -eo pipefail in the checkout (build/agent) or
the task's working directory (shell). Example:

  grove dispatch --pool linux --repo https://github.com/acme/widget --ref main -- make test`,
		RunE: func(cmd *cobra.Command, args []string) error {
			script := strings.Join(args, " ")
			if strings.TrimSpace(script) == "" {
				return fmt.Errorf("dispatch: a script is required after --")
			}

			env := map[string]string{}
			for _, kv := range envs {
				k, v, ok := strings.Cut(kv, "=")
				if !ok {
					return fmt.Errorf("dispatch: --env %q must be KEY=VALUE", kv)
				}
				env[k] = v
			}

			k := dispatch.Kind(kind)
			switch k {
			case dispatch.KindBuild, dispatch.KindAgent, dispatch.KindShell:
			default:
				return fmt.Errorf("dispatch: --kind must be build|agent|shell, got %q", kind)
			}

			client, err := newAPIClient()
			if err != nil {
				return err
			}

			req := dispatch.JobRequest{
				Kind:      k,
				Pool:      pool,
				Repo:      repo,
				Ref:       ref,
				Script:    script,
				Env:       env,
				Timeout:   dispatch.Duration(timeout),
				Requester: "cli",
			}
			id, err := client.SubmitJob(cmd.Context(), req)
			if err != nil {
				return fmt.Errorf("dispatch: submit: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), id)

			if follow {
				return streamLogsToStdout(cmd, client, id, true)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "target pool (required), e.g. linux, macos, gpu")
	cmd.Flags().StringVar(&kind, "kind", string(dispatch.KindShell), "job kind: build|agent|shell")
	cmd.Flags().StringVar(&repo, "repo", "", "git repo URL (required for build/agent)")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref: branch, tag or commit SHA")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "environment variable KEY=VALUE (repeatable)")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "job timeout, e.g. 30m")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream logs until the job finishes")
	_ = cmd.MarkFlagRequired("pool")
	Root.AddCommand(cmd)
}
