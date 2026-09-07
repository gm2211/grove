package install

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/gm2211/grove/internal/config"
)

// buildClientSteps assembles the client role plan: just a config.yaml pointing at the grove
// server, no local services.
func buildClientSteps(_ Runner, opts Options, _ io.Writer) []Step {
	cfgPath := filepath.Join(opts.ConfigDir(), "config.yaml")
	return []Step{
		{
			Name:        "config:client-config-yaml",
			Description: "Write config.yaml with the grove server URL and token (no local services for the client role).",
			Check: func(ctx context.Context) (bool, error) {
				cfg, err := config.LoadFrom(cfgPath)
				if err != nil {
					if err == config.ErrNotFound {
						return false, nil
					}
					return false, err
				}
				return cfg.Server.URL == opts.ServerURL && (opts.Token == "" || cfg.Server.Token == opts.Token), nil
			},
			Apply: func(ctx context.Context) error {
				if opts.ServerURL == "" {
					return errors.New("--server is required for --role client")
				}
				cfg := &config.Config{
					Server: config.ServerConfig{
						URL:   opts.ServerURL,
						Token: opts.Token,
					},
				}
				return config.Save(cfgPath, cfg)
			},
		},
	}
}
