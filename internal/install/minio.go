package install

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gm2211/grove/internal/config"
)

const (
	// artifactBucket is the bucket grove's build jobs upload ./artifacts/** to.
	artifactBucket = "grove"
	// minioClientBinName avoids colliding with Midnight Commander's `mc` on Linux.
	minioClientBinName = "grove-mc"
	// minioAdminAlias is the mc alias holding MinIO's root credential, in grove's own mc config dir.
	minioAdminAlias = "grove-root"
	// minioArtifactPolicy is the bucket-scoped policy attached to the artifact user.
	minioArtifactPolicy = "grove-artifacts"
)

// minioReadyTimeout bounds how long the installer waits for a freshly started MinIO.
var minioReadyTimeout = 2 * time.Minute

func minioClientPath(opts Options) string {
	return filepath.Join(orchardBinDir(opts), minioClientBinName)
}

func minioEnvPath(opts Options) string { return filepath.Join(opts.ConfigDir(), "minio", "env") }

// minioClientStep installs the pinned, checksum-verified MinIO client the control plane uses to
// create the artifact user. Same download on macOS and Linux, so both get the same version.
func minioClientStep(r Runner, opts Options) Step {
	dest := minioClientPath(opts)
	return Step{
		Name:        "minio-client-binary",
		Description: fmt.Sprintf("Install the MinIO client %s as %s, verified against grove's pinned SHA-256.", MinIOClientVersion, dest),
		Check: func(ctx context.Context) (bool, error) {
			_, err := os.Stat(dest)
			return err == nil, nil
		},
		Apply: func(ctx context.Context) error {
			d, err := minioClientDownload(opts)
			if err != nil {
				return err
			}
			return installVerifiedBinary(ctx, r, opts, d, dest)
		},
	}
}

// readMinIORootCredentials parses MINIO_ROOT_USER / MINIO_ROOT_PASSWORD from the env file grove
// renders (see RenderMinIOEnv).
func readMinIORootCredentials(path string) (user, password string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch k {
		case "MINIO_ROOT_USER":
			user = v
		case "MINIO_ROOT_PASSWORD":
			password = v
		}
	}
	if err := sc.Err(); err != nil {
		return "", "", err
	}
	if user == "" || password == "" {
		return "", "", fmt.Errorf("%s has no MINIO_ROOT_USER/MINIO_ROOT_PASSWORD", path)
	}
	return user, password, nil
}

// minioArtifactUserStep gives grove a MinIO user that can only read/write objects in the artifact
// bucket, and records THAT credential (not MinIO's root one) in config.yaml's artifacts section.
// Secrets go to mc over stdin, never argv. Satisfied once config.yaml holds a non-root credential,
// so an install that predates this step (root credential in config.yaml) is migrated on re-run.
func minioArtifactUserStep(r Runner, opts Options) Step {
	cfgDir := opts.ConfigDir()
	envPath := minioEnvPath(opts)
	mc := minioClientPath(opts)
	mcConfigDir := filepath.Join(cfgDir, "minio", "mc")
	policyPath := filepath.Join(cfgDir, "minio", "artifact-policy.json")
	return Step{
		Name:        "minio-artifact-user",
		Description: fmt.Sprintf("Create a MinIO user restricted to the %q bucket (policy %s) and store its credential, not MinIO's root credential, in config.yaml.", artifactBucket, minioArtifactPolicy),
		Check: func(ctx context.Context) (bool, error) {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return false, err
			}
			if cfg.Artifacts.AccessKey == "" || cfg.Artifacts.SecretKey == "" {
				return false, nil
			}
			rootUser, _, err := readMinIORootCredentials(envPath)
			if err != nil {
				// No local MinIO env (artifacts pointed elsewhere by hand): nothing to scope.
				return true, nil
			}
			return cfg.Artifacts.AccessKey != rootUser, nil
		},
		Apply: func(ctx context.Context) error {
			cfg, err := loadOrInitConfig(cfgDir)
			if err != nil {
				return err
			}
			endpoint, bucket := cfg.Artifacts.Endpoint, cfg.Artifacts.Bucket
			if endpoint == "" {
				return errors.New("config.yaml has no artifacts.endpoint; the config:minio-env step must run first")
			}
			if bucket == "" {
				bucket = artifactBucket
			}
			rootUser, rootPassword, err := readMinIORootCredentials(envPath)
			if err != nil {
				return err
			}
			mcRun := func(args ...string) error {
				_, _, err := r.Run(ctx, mc, append([]string{"--config-dir", mcConfigDir}, args...)...)
				return err
			}
			if err := os.MkdirAll(mcConfigDir, 0o700); err != nil {
				return err
			}
			// --api skips mc's signature probe, so the alias is recorded even if MinIO is still
			// starting; `mc ready` then waits for it.
			if _, _, err := r.RunWithStdin(ctx, rootUser+"\n"+rootPassword+"\n", mc,
				"--config-dir", mcConfigDir, "alias", "set", minioAdminAlias, endpoint, "--api", "S3v4"); err != nil {
				return fmt.Errorf("configure MinIO admin alias: %w", err)
			}
			readyCtx, cancel := context.WithTimeout(ctx, minioReadyTimeout)
			_, _, err = r.Run(readyCtx, mc, "--config-dir", mcConfigDir, "ready", minioAdminAlias)
			cancel()
			if err != nil {
				return fmt.Errorf("wait for MinIO at %s: %w", endpoint, err)
			}
			if err := mcRun("mb", "--ignore-existing", minioAdminAlias+"/"+bucket); err != nil {
				return fmt.Errorf("create artifact bucket: %w", err)
			}
			policy, err := RenderMinIOArtifactPolicy(MinIOArtifactPolicySpec{Bucket: bucket})
			if err != nil {
				return err
			}
			if err := writeFile(policyPath, policy); err != nil {
				return err
			}
			if err := mcRun("admin", "policy", "create", minioAdminAlias, minioArtifactPolicy, policyPath); err != nil {
				return fmt.Errorf("create MinIO policy %s: %w", minioArtifactPolicy, err)
			}
			suffix, err := randomHex(8)
			if err != nil {
				return err
			}
			accessKey := "grove-artifacts-" + suffix
			secretKey, err := randomHex(32)
			if err != nil {
				return err
			}
			if _, _, err := r.RunWithStdin(ctx, secretKey+"\n", mc,
				"--config-dir", mcConfigDir, "admin", "user", "add", minioAdminAlias, accessKey); err != nil {
				return fmt.Errorf("create MinIO artifact user: %w", err)
			}
			if err := mcRun("admin", "policy", "attach", minioAdminAlias, minioArtifactPolicy, "--user", accessKey); err != nil &&
				!strings.Contains(err.Error(), "already in effect") {
				return fmt.Errorf("attach MinIO policy %s: %w", minioArtifactPolicy, err)
			}
			return updateConfig(cfgDir, func(cfg *config.Config) {
				cfg.Artifacts.Bucket = bucket
				cfg.Artifacts.AccessKey = accessKey
				cfg.Artifacts.SecretKey = secretKey
			})
		},
	}
}
