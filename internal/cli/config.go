package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gm2211/grove/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func init() {
	Root.AddCommand(configCmd)
	configCmd.AddCommand(configInitCmd)
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
}

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage grove's configuration (~/.config/grove/config.yaml).",
}

var configInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Write a starter config file if none exists yet.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}

		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config already exists at %s (edit it directly, or `grove config set`)", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}

		if err := config.Save(path, &config.Config{}); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)

		return nil
	},
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the resolved configuration.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, path, err := loadConfig()
		if err != nil {
			return err
		}

		out, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "# %s\n%s", path, out)

		return nil
	},
}

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a single configuration key (dotted path, e.g. orchard.url) and save.",
	Long: "Set a single configuration key (dotted path, e.g. orchard.url) and save.\n\n" +
		"A value of - reads the value from standard input instead, so a secret such as\n" +
		"tailscale.authKey never appears in the process list or shell history.",
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		path, err := config.DefaultPath()
		if err != nil {
			return err
		}

		cfg, err := config.LoadFrom(path)
		if err != nil {
			if !errors.Is(err, config.ErrNotFound) {
				return err
			}
			cfg = &config.Config{}
		}

		value := args[1]
		if value == "-" {
			if value, err = readConfigValue(cmd.InOrStdin()); err != nil {
				return err
			}
		}

		if err := setConfigKey(cfg, args[0], value); err != nil {
			return err
		}

		if err := config.Save(path, cfg); err != nil {
			return err
		}

		fmt.Fprintf(cmd.OutOrStdout(), "set %s in %s\n", args[0], path)

		return nil
	},
}

// setConfigKey applies value to the field named by a dotted key path (e.g. "orchard.url").
func setConfigKey(cfg *config.Config, key, value string) error {
	switch key {
	case "orchard.url":
		cfg.Orchard.URL = value
	case "orchard.token":
		cfg.Orchard.Token = value
	case "nomad.url":
		cfg.Nomad.URL = value
	case "nomad.token":
		cfg.Nomad.Token = value
	case "server.listen":
		cfg.Server.Listen = value
	case "server.url":
		cfg.Server.URL = value
	case "server.token":
		cfg.Server.Token = value
	case "artifacts.endpoint":
		cfg.Artifacts.Endpoint = value
	case "artifacts.bucket":
		cfg.Artifacts.Bucket = value
	case "artifacts.accessKey":
		cfg.Artifacts.AccessKey = value
	case "artifacts.secretKey":
		cfg.Artifacts.SecretKey = value
	case "fleet":
		cfg.Fleet = value
	case "tailscale.authKey":
		cfg.Tailscale.AuthKey = value
	case "tailscale.tags":
		cfg.Tailscale.Tags = splitList(value)
	default:
		return fmt.Errorf("unknown config key %q", key)
	}

	return nil
}

// readConfigValue reads one value from r for `grove config set <key> -`: everything up to EOF,
// minus the trailing newline a pipe or `echo` adds. An empty value is refused rather than
// silently clearing the key.
func readConfigValue(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 64<<10))
	if err != nil {
		return "", fmt.Errorf("read value from stdin: %w", err)
	}
	value := strings.TrimRight(string(data), "\r\n")
	if value == "" {
		return "", errors.New("no value on stdin")
	}
	return value, nil
}

// splitList parses a comma-separated config value, e.g. "tag:grove-vm,tag:ci", dropping blanks.
func splitList(value string) []string {
	var out []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
