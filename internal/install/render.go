package install

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/gm2211/grove/internal/nomad"
)

// LaunchAgentSpec renders a macOS launchd plist: a per-user LaunchAgent, or, with UserName, a
// system LaunchDaemon that runs as that user.
type LaunchAgentSpec struct {
	Label string
	// UserName is the account a LaunchDaemon runs as. Without it a daemon runs as root, so grove's
	// worker daemons always set it (see daemonPlist) and never to root.
	UserName                    string
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
{{- if .UserName}}
	<key>UserName</key>
	<string>{{.UserName}}</string>
{{- end}}
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

// NomadTLSFiles names the PEM files a Nomad agent presents and trusts (see internal/pki).
type NomadTLSFiles struct {
	CA   string
	Cert string
	Key  string
}

// NomadServerConfigSpec is what grove needs to render a single-node Nomad server config.
type NomadServerConfigSpec struct {
	DataDir  string
	BindAddr string
	TLS      NomadTLSFiles
}

var nomadServerConfigTemplate = template.Must(template.New("nomad").Parse(`# Managed by grove install. Re-run ` + "`grove install --role control-plane`" + ` to regenerate.
data_dir  = "{{.DataDir}}"
bind_addr = "{{.BindAddr}}"

server {
  enabled          = true
  bootstrap_expect = 1

  # A client registers only with an introduction token bound to its node name and pool, minted
  # by grove for that one VM (or for the sandbox-host client below). A CA-signed certificate
  # alone does not let a machine join.
  client_introduction {
    enforcement = "strict"
  }
}

# Mutual TLS on RPC and HTTP: the server only talks to peers holding a certificate grove's CA
# signed. Without it anything that reached port 4647 could register as a node and be handed jobs.
# verify_server_hostname makes Nomad check the certificate's role name (server.global.nomad /
# client.global.nomad), so a client certificate cannot pose as a server. The CA key never leaves
# this machine.
tls {
  http                   = true
  rpc                    = true
  ca_file                = "{{.TLS.CA}}"
  cert_file              = "{{.TLS.Cert}}"
  key_file               = "{{.TLS.Key}}"
  verify_server_hostname = true
  verify_https_client    = true
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

// SandboxClientNodeName is the Nomad node name of the control plane's own client, the one that
// runs `grove sandbox run` for sealed pools. Its introduction token is bound to this name.
const SandboxClientNodeName = nomad.SandboxHostNodeName

// Loopback ports of the sandbox-host client agent, clear of the server's 4646-4648.
const (
	sandboxClientHTTPPort = 4656
	sandboxClientRPCPort  = 4657
	sandboxClientSerfPort = 4658
)

// NomadSandboxClientConfigSpec renders the control plane's sandbox-host Nomad client.
type NomadSandboxClientConfigSpec struct {
	DataDir string
	// Server is the server's RPC address, host:port.
	Server string
	TLS    NomadTLSFiles
}

var nomadSandboxClientConfigTemplate = template.Must(template.New("nomad-sandbox-client").Parse(`# Managed by grove install. Re-run ` + "`grove install --role control-plane`" + ` to regenerate.
#
# The control plane's own Nomad client, for one kind of job only: ` + "`grove sandbox run`" + `, which drives
# a sealed pool's job in its VM from here (nomad/jobs/sandbox.nomad.hcl), since a sealed VM can't
# reach Nomad. This node advertises grove_sandbox_host and no pool, so no other grove job is ever
# placed on it. It is a separate agent from the server because the server enforces client
# introduction for every client, its own included.
name      = "` + SandboxClientNodeName + `"
data_dir  = "{{.DataDir}}"
bind_addr = "127.0.0.1"

ports {
  http = {{.HTTPPort}}
  rpc  = {{.RPCPort}}
  serf = {{.SerfPort}}
}

advertise {
  http = "127.0.0.1:{{.HTTPPort}}"
  rpc  = "127.0.0.1:{{.RPCPort}}"
  serf = "127.0.0.1:{{.SerfPort}}"
}

client {
  enabled = true
  servers = ["{{.Server}}"]
  meta {
    grove_sandbox_host = "true"
  }
}

plugin "raw_exec" {
  config {
    enabled = true
  }
}

tls {
  rpc                    = true
  ca_file                = "{{.TLS.CA}}"
  cert_file              = "{{.TLS.Cert}}"
  key_file               = "{{.TLS.Key}}"
  verify_server_hostname = true
}

acl {
  enabled = true
}
`))

// RenderNomadSandboxClientConfig renders the sandbox-host client's config. Deterministic.
func RenderNomadSandboxClientConfig(spec NomadSandboxClientConfigSpec) (string, error) {
	var buf bytes.Buffer
	data := struct {
		NomadSandboxClientConfigSpec
		HTTPPort, RPCPort, SerfPort int
	}{spec, sandboxClientHTTPPort, sandboxClientRPCPort, sandboxClientSerfPort}
	if err := nomadSandboxClientConfigTemplate.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render nomad sandbox client config: %w", err)
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
