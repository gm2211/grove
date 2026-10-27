package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gm2211/grove/internal/install"
)

var registryLoginFlags struct {
	registry string
	user     string
}

var registryCmd = &cobra.Command{
	Use:   "registry",
	Short: "Log this worker in to the registry its VM images come from.",
}

var registryLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Log this worker in to a private image registry (`tart login`, token over stdin).",
	Long: `grove registry login logs the macOS user running this worker in to a container registry, so
the worker can pull grove's private VM images (ghcr.io by default). Run it as the user the worker
runs as. Tart keeps the token in that user's keychain; grove records only the user and registry
(~/.config/grove/registry-login.<registry>).

The token is read from $GROVE_REGISTRY_TOKEN, or else from stdin, never from an argument. On a
Mac, copy the token and pipe it in so it stays out of your shell history:

  pbpaste | grove registry login --user <github-username>

For GHCR, use a classic personal access token with only the read:packages scope. Running this
again replaces the token, so it is also how you rotate one.`,
	Args: cobra.NoArgs,
	RunE: runRegistryLogin,
}

func init() {
	registryLoginCmd.Flags().StringVar(&registryLoginFlags.registry, "registry", "ghcr.io", "container registry to log in to")
	registryLoginCmd.Flags().StringVar(&registryLoginFlags.user, "user", "", "registry username, e.g. your GitHub login (required)")
	_ = registryLoginCmd.MarkFlagRequired("user")
	registryCmd.AddCommand(registryLoginCmd)
	Root.AddCommand(registryCmd)
}

func runRegistryLogin(cmd *cobra.Command, _ []string) error {
	token, err := readRegistryToken(os.Getenv("GROVE_REGISTRY_TOKEN"), cmd.InOrStdin(), stdinIsTerminal())
	if err != nil {
		return err
	}
	opts := install.Options{
		Role:          install.RoleWorker,
		Registry:      registryLoginFlags.registry,
		RegistryUser:  registryLoginFlags.user,
		RegistryToken: token,
	}
	if err := install.RegistryLogin(cmd.Context(), install.ExecRunner{}, opts); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s as %s.\n", registryLoginFlags.registry, registryLoginFlags.user)
	return nil
}

// readRegistryToken takes the token from env, or else from piped stdin. It refuses to read a
// terminal, where the token would be echoed as it is typed.
func readRegistryToken(env string, in io.Reader, interactive bool) (string, error) {
	if token := strings.TrimSpace(env); token != "" {
		return token, nil
	}
	if interactive {
		return "", errors.New("no token: copy it, then run `pbpaste | grove registry login --user <you>`, or set GROVE_REGISTRY_TOKEN")
	}
	b, err := io.ReadAll(io.LimitReader(in, 64<<10))
	if err != nil {
		return "", fmt.Errorf("reading the token from stdin: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", errors.New("no token: stdin was empty and GROVE_REGISTRY_TOKEN is unset")
	}
	return token, nil
}
