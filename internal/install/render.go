package install

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"
)

// LaunchAgentSpec renders a macOS launchd agent plist.
type LaunchAgentSpec struct {
	Label                       string
	Program                     string
	Args                        []string
	WorkingDir                  string
	Env                         map[string]string
	AssociatedBundleIdentifiers []string
	StdoutPath                  string
	StderrPath                  string
	KeepAlive                   bool
	RunAtLoad                   bool
}

var launchAgentTemplate = template.Must(template.New("launchagent").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Program}}</string>
{{- range .Args}}
		<string>{{.}}</string>
{{- end}}
	</array>
{{- if .WorkingDir}}
	<key>WorkingDirectory</key>
	<string>{{.WorkingDir}}</string>
{{- end}}
{{- if .Env}}
	<key>EnvironmentVariables</key>
	<dict>
{{- range $k, $v := .Env}}
		<key>{{$k}}</key>
		<string>{{$v}}</string>
{{- end}}
	</dict>
{{- end}}
{{- if .AssociatedBundleIdentifiers}}
	<key>AssociatedBundleIdentifiers</key>
	<array>
{{- range .AssociatedBundleIdentifiers}}
		<string>{{.}}</string>
{{- end}}
	</array>
{{- end}}
	<key>KeepAlive</key>
	<{{if .KeepAlive}}true{{else}}false{{end}}/>
	<key>RunAtLoad</key>
	<{{if .RunAtLoad}}true{{else}}false{{end}}/>
	<key>StandardOutPath</key>
	<string>{{.StdoutPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.StderrPath}}</string>
</dict>
</plist>
`))

// RenderLaunchAgent renders a launchd agent plist. Deterministic: safe for golden tests.
func RenderLaunchAgent(spec LaunchAgentSpec) (string, error) {
	var buf bytes.Buffer
	if err := launchAgentTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render launch agent %s: %w", spec.Label, err)
	}
	return buf.String(), nil
}

// SystemdUnitSpec renders a Linux systemd --user service unit.
type SystemdUnitSpec struct {
	Description string
	Program     string
	Args        []string
	WorkingDir  string
	Env         map[string]string
}

var systemdUnitTemplate = template.Must(template.New("systemd").Parse(`# Managed by grove install. Re-run ` + "`grove install`" + ` to regenerate; do not edit by hand.
[Unit]
Description={{.Description}}
After=network-online.target
Wants=network-online.target

[Service]
ExecStart={{.Program}}{{range .Args}} {{.}}{{end}}
Restart=always
RestartSec=5
{{- if .WorkingDir}}
WorkingDirectory={{.WorkingDir}}
{{- end}}
{{- range $k, $v := .Env}}
Environment={{$k}}={{$v}}
{{- end}}

[Install]
WantedBy=default.target
`))

// RenderSystemdUnit renders a systemd --user unit. Deterministic: safe for golden tests.
func RenderSystemdUnit(spec SystemdUnitSpec) (string, error) {
	var buf bytes.Buffer
	if err := systemdUnitTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render systemd unit %s: %w", spec.Description, err)
	}
	return buf.String(), nil
}

// NomadServerConfigSpec is what grove needs to render a single-node Nomad server config.
type NomadServerConfigSpec struct {
	DataDir  string
	BindAddr string
}

var nomadServerConfigTemplate = template.Must(template.New("nomad").Parse(`# Managed by grove install. Re-run ` + "`grove install --role control-plane`" + ` to regenerate.
data_dir  = "{{.DataDir}}"
bind_addr = "{{.BindAddr}}"

server {
  enabled          = true
  bootstrap_expect = 1
}

acl {
  # On: anything on the tailnet (or inside a VM) that reaches this API without a token gets
  # nothing. grove holds the management token (config.yaml nomad.token); each VM gets its own
  # node-only token to drain itself on recycle.
  enabled = true
}
`))

// RenderNomadServerConfig renders nomad's server.hcl. Deterministic: safe for golden tests.
func RenderNomadServerConfig(spec NomadServerConfigSpec) (string, error) {
	var buf bytes.Buffer
	if err := nomadServerConfigTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render nomad server config: %w", err)
	}
	return buf.String(), nil
}

// MinIOEnvSpec is the env file grove renders for a single-node MinIO instance.
type MinIOEnvSpec struct {
	RootUser     string
	RootPassword string
	DataDir      string
	Address      string
}

var minioEnvTemplate = template.Must(template.New("minio").Parse(`# Managed by grove install. Re-run ` + "`grove install --role control-plane`" + ` to regenerate.
MINIO_ROOT_USER={{.RootUser}}
MINIO_ROOT_PASSWORD={{.RootPassword}}
MINIO_VOLUMES={{.DataDir}}
MINIO_ADDRESS={{.Address}}
`))

// RenderMinIOEnv renders MinIO's env file. Deterministic: safe for golden tests.
func RenderMinIOEnv(spec MinIOEnvSpec) (string, error) {
	var buf bytes.Buffer
	if err := minioEnvTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render minio env: %w", err)
	}
	return buf.String(), nil
}

// OrchardControllerEnvSpec is the env file for the Orchard controller process.
type OrchardControllerEnvSpec struct {
	Home    string
	Address string
}

var orchardControllerEnvTemplate = template.Must(template.New("orchard-controller").Parse(`# Managed by grove install. Re-run ` + "`grove install --role control-plane`" + ` to regenerate.
ORCHARD_HOME={{.Home}}
ORCHARD_ADDRESS={{.Address}}
`))

// RenderOrchardControllerEnv renders the Orchard controller's env file.
func RenderOrchardControllerEnv(spec OrchardControllerEnvSpec) (string, error) {
	var buf bytes.Buffer
	if err := orchardControllerEnvTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render orchard controller env: %w", err)
	}
	return buf.String(), nil
}

// MinIOArtifactPolicySpec names the one bucket grove's artifact credential may touch.
type MinIOArtifactPolicySpec struct {
	Bucket string
}

var minioArtifactPolicyTemplate = template.Must(template.New("minio-artifact-policy").Parse(`{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetBucketLocation",
        "s3:ListBucket",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": ["arn:aws:s3:::{{.Bucket}}"]
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
      "Resource": ["arn:aws:s3:::{{.Bucket}}/*"]
    }
  ]
}
`))

// RenderMinIOArtifactPolicy renders the IAM policy attached to grove's artifact user: object
// read/write on Bucket only — no admin API, no other bucket, no bucket creation or deletion.
// Deterministic: safe for golden tests.
func RenderMinIOArtifactPolicy(spec MinIOArtifactPolicySpec) (string, error) {
	if spec.Bucket == "" || strings.ContainsAny(spec.Bucket, "\"\\*/ ") {
		return "", fmt.Errorf("render minio artifact policy: invalid bucket name %q", spec.Bucket)
	}
	var buf bytes.Buffer
	if err := minioArtifactPolicyTemplate.Execute(&buf, spec); err != nil {
		return "", fmt.Errorf("render minio artifact policy: %w", err)
	}
	return buf.String(), nil
}
