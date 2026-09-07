package install

import (
	"bytes"
	"fmt"
	"text/template"
)

// LaunchAgentSpec renders a macOS launchd agent plist.
type LaunchAgentSpec struct {
	Label      string
	Program    string
	Args       []string
	WorkingDir string
	Env        map[string]string
	StdoutPath string
	StderrPath string
	KeepAlive  bool
	RunAtLoad  bool
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
  # Off: the control plane is reachable only over the tailnet, nothing routes here from outside.
  enabled = false
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
