package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/gm2211/grove/internal/install"
	"github.com/spf13/cobra"
)

var workerRememberLoginForget bool

var workerRememberLoginCmd = &cobra.Command{
	Use:   "remember-login",
	Short: "Let the worker unlock this user's login keychain itself, so macOS VMs boot with nobody logged in.",
	Long: `A macOS VM boots only while the worker user's login keychain is unlocked: Virtualization.framework
keeps its host key there. A GUI login unlocks it, but a login ends at every reboot or crash, and
then every macOS VM fails with "Failed to get current host key" until someone logs in again.

Run this once, as the user the worker runs as (for example: sudo -u argos -H grove worker
remember-login). It checks the login password against that user's keychain and keeps it in
~/.config/grove/credentials/login-keychain (0600, readable only by that user and root). The
worker's wrapper then unlocks the login keychain inside the worker daemon's own session before
Orchard starts, and again every 5 minutes. Re-run it after changing that user's password.

The password is read from stdin (pipe it in), or from the terminal without echo. It never appears
in a process's arguments. --forget removes it again.`,
	Args: cobra.NoArgs,
	RunE: runWorkerRememberLogin,
}

func init() {
	workerRememberLoginCmd.Flags().BoolVar(&workerRememberLoginForget, "forget", false, "remove the stored password")
	workerCmd.AddCommand(workerRememberLoginCmd)
}

func runWorkerRememberLogin(cmd *cobra.Command, _ []string) error {
	opts := install.Options{Role: install.RoleWorker}
	if workerRememberLoginForget {
		if err := install.ForgetLogin(opts); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Removed the stored login password. After the next reboot, macOS VMs need this user logged in again.")
		return nil
	}
	password, err := readLoginPassword(cmd.InOrStdin(), cmd.ErrOrStderr(), stdinIsTerminal())
	if err != nil {
		return err
	}
	if err := install.RememberLogin(cmd.Context(), install.ExecRunner{}, opts, password); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Stored. The worker now unlocks this user's login keychain itself (restarted it), so macOS VMs boot with nobody logged in, after reboots too.")
	return nil
}

// readLoginPassword reads one line: from a terminal with echo off, or from piped stdin.
func readLoginPassword(in io.Reader, prompt io.Writer, interactive bool) (string, error) {
	if interactive {
		fmt.Fprint(prompt, "Login password of this user (not echoed): ")
		off := exec.Command("/bin/stty", "-echo")
		off.Stdin = os.Stdin
		if err := off.Run(); err != nil {
			return "", fmt.Errorf("turn terminal echo off: %w", err)
		}
		defer func() {
			on := exec.Command("/bin/stty", "echo")
			on.Stdin = os.Stdin
			_ = on.Run()
			fmt.Fprintln(prompt)
		}()
	}
	line, err := bufio.NewReader(io.LimitReader(in, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("reading the password: %w", err)
	}
	password := strings.TrimRight(line, "\r\n")
	if password == "" {
		return "", errors.New("no password: pipe it in, or run this in a terminal")
	}
	return password, nil
}
