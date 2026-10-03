package install

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gm2211/grove/internal/config"
)

func TestRenderMinIOArtifactPolicy_ScopedToOneBucket(t *testing.T) {
	got, err := RenderMinIOArtifactPolicy(MinIOArtifactPolicySpec{Bucket: "grove"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetBucketLocation",
        "s3:ListBucket",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": ["arn:aws:s3:::grove"]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:AbortMultipartUpload",
        "s3:DeleteObject",
        "s3:GetObject",
        "s3:ListMultipartUploadParts",
        "s3:PutObject"
      ],
      "Resource": ["arn:aws:s3:::grove/*"]
    }
  ]
}
`
	if got != want {
		t.Errorf("policy mismatch:\n%s", got)
	}
	for _, bad := range []string{"", "*", "a/b", `x"y`} {
		if _, err := RenderMinIOArtifactPolicy(MinIOArtifactPolicySpec{Bucket: bad}); err == nil {
			t.Errorf("bucket %q should be rejected", bad)
		}
	}
}

func runControlPlanePlan(t *testing.T, home, goos string) (*FakeRunner, string) {
	t.Helper()
	r := NewFakeRunner()
	scriptWorkerEnrollmentToken(r, home)
	scriptTailscaleUp(r)
	opts := testControlPlaneOptions(home, goos)
	scriptPinnedDownloads(r, opts)
	steps, err := BuildPlan(r, opts, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RunPlan(context.Background(), steps, PlanOptions{Out: io.Discard}); err != nil {
		t.Fatalf("RunPlan: %v", err)
	}
	return r, filepath.Join(home, ".config", "grove")
}

func TestControlPlanePlan_ArtifactCredentialIsScopedUserNotRoot(t *testing.T) {
	home := t.TempDir()
	r, cfgDir := runControlPlanePlan(t, home, "darwin")

	rootUser, rootPassword, err := readMinIORootCredentials(filepath.Join(cfgDir, "minio", "env"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(filepath.Join(cfgDir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Artifacts
	if a.AccessKey == rootUser || a.SecretKey == rootPassword {
		t.Fatal("config.yaml must not carry MinIO's root credential")
	}
	if !strings.HasPrefix(a.AccessKey, "grove-artifacts-") || len(a.SecretKey) != 64 || a.Bucket != "grove" {
		t.Fatalf("unexpected artifact credential: %+v", a)
	}

	mc := filepath.Join(home, ".local", "bin", "grove-mc")
	mcCfg := filepath.Join(cfgDir, "minio", "mc")
	alias := mc + " --config-dir " + mcCfg + " alias set grove-root " + a.Endpoint + " --api S3v4"
	if stdin, ok := r.StdinFor(alias); !ok || stdin != rootUser+"\n"+rootPassword+"\n" {
		t.Fatalf("root credential must reach mc alias set over stdin; ok=%v", ok)
	}
	userAdd := mc + " --config-dir " + mcCfg + " admin user add grove-root " + a.AccessKey
	if stdin, ok := r.StdinFor(userAdd); !ok || stdin != a.SecretKey+"\n" {
		t.Fatalf("artifact secret must reach mc admin user add over stdin; ok=%v", ok)
	}
	policyPath := filepath.Join(cfgDir, "minio", "artifact-policy.json")
	assertBefore(t, r, mc+" --config-dir "+mcCfg+" ready grove-root", mc+" --config-dir "+mcCfg+" mb --ignore-existing grove-root/grove")
	assertBefore(t, r, mc+" --config-dir "+mcCfg+" admin policy create grove-root grove-artifacts "+policyPath, userAdd)
	if !r.CalledWith(mc + " --config-dir " + mcCfg + " admin policy attach grove-root grove-artifacts --user " + a.AccessKey) {
		t.Error("expected the bucket-scoped policy to be attached to the artifact user")
	}
	for _, c := range r.Calls {
		for _, arg := range c.Args {
			if arg == rootPassword || arg == a.SecretKey {
				t.Fatalf("secret leaked into argv: %s", c.String())
			}
		}
	}
	// The pinned mc binary is installed through the verified path on macOS too.
	staged := filepath.Join(downloadDir(testControlPlaneOptions(home, "darwin")), "mc."+MinIOClientVersion)
	assertBefore(t, r, "shasum -a 256 "+staged, "install -m 0755 "+staged+" "+mc)
	if info, err := os.Stat(policyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("policy file: %v %v", info, err)
	}
}

func TestMinIOArtifactUserStep_MigratesRootCredentialInConfig(t *testing.T) {
	home := t.TempDir()
	opts := testControlPlaneOptions(home, "linux")
	cfgDir := opts.ConfigDir()
	if err := writeFile(minioEnvPath(opts), "MINIO_ROOT_USER=rootuser\nMINIO_ROOT_PASSWORD=rootpass\n"); err != nil {
		t.Fatal(err)
	}
	// What installs before this change wrote: the root credential as the artifact credential.
	if err := updateConfig(cfgDir, func(c *config.Config) {
		c.Artifacts = config.ArtifactsConfig{Endpoint: "http://100.64.0.1:9000", Bucket: "grove", AccessKey: "rootuser", SecretKey: "rootpass"}
	}); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	step := minioArtifactUserStep(r, opts)
	ok, err := step.Check(context.Background())
	if err != nil || ok {
		t.Fatalf("root credential in config.yaml must not satisfy the step (ok=%v err=%v)", ok, err)
	}
	if err := step.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ok, _ := step.Check(context.Background()); !ok {
		t.Fatal("step should be satisfied after migration")
	}
	cfg, _ := loadOrInitConfig(cfgDir)
	if cfg.Artifacts.AccessKey == "rootuser" || cfg.Artifacts.Endpoint != "http://100.64.0.1:9000" {
		t.Fatalf("artifacts after migration: %+v", cfg.Artifacts)
	}
}

func TestMinIOArtifactUserStep_FailureKeepsConfigUntouched(t *testing.T) {
	home := t.TempDir()
	opts := testControlPlaneOptions(home, "linux")
	cfgDir := opts.ConfigDir()
	if err := writeFile(minioEnvPath(opts), "MINIO_ROOT_USER=rootuser\nMINIO_ROOT_PASSWORD=rootpass\n"); err != nil {
		t.Fatal(err)
	}
	if err := updateConfig(cfgDir, func(c *config.Config) {
		c.Artifacts = config.ArtifactsConfig{Endpoint: "http://100.64.0.1:9000", Bucket: "grove"}
	}); err != nil {
		t.Fatal(err)
	}
	r := NewFakeRunner()
	mc := minioClientPath(opts)
	r.Script(mc+" --config-dir "+filepath.Join(cfgDir, "minio", "mc")+" ready grove-root", FakeResult{Err: io.ErrUnexpectedEOF})
	if err := minioArtifactUserStep(r, opts).Apply(context.Background()); err == nil {
		t.Fatal("expected Apply to fail when MinIO never becomes ready")
	}
	cfg, _ := loadOrInitConfig(cfgDir)
	if cfg.Artifacts.AccessKey != "" {
		t.Fatal("no credential may be recorded when the user was not created")
	}
}
