package install

import (
	"strings"
	"testing"
)

func TestRenderLaunchAgent(t *testing.T) {
	got, err := RenderLaunchAgent(LaunchAgentSpec{
		Label:                       "com.grove.orchard-worker",
		Program:                     "/Users/alice/.local/bin/orchard",
		Args:                        []string{"worker", "run", "--name", "alices-mac", "https://grove-cp.tailnet.ts.net:6120"},
		WorkingDir:                  "/Users/alice",
		Env:                         map[string]string{"B_VAR": "2", "A_VAR": "1"},
		AssociatedBundleIdentifiers: []string{"com.grove.orchard-worker"},
		StdoutPath:                  "/Users/alice/Library/Logs/grove/orchard-worker.log",
		StderrPath:                  "/Users/alice/Library/Logs/grove/orchard-worker.err.log",
		KeepAlive:                   true,
		RunAtLoad:                   true,
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgent: %v", err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.grove.orchard-worker</string>
	<key>ProgramArguments</key>
	<array>
		<string>/Users/alice/.local/bin/orchard</string>
		<string>worker</string>
		<string>run</string>
		<string>--name</string>
		<string>alices-mac</string>
		<string>https://grove-cp.tailnet.ts.net:6120</string>
	</array>
	<key>WorkingDirectory</key>
	<string>/Users/alice</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>A_VAR</key>
		<string>1</string>
		<key>B_VAR</key>
		<string>2</string>
	</dict>
	<key>AssociatedBundleIdentifiers</key>
	<array>
		<string>com.grove.orchard-worker</string>
	</array>
	<key>KeepAlive</key>
	<true/>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>/Users/alice/Library/Logs/grove/orchard-worker.log</string>
	<key>StandardErrorPath</key>
	<string>/Users/alice/Library/Logs/grove/orchard-worker.err.log</string>
</dict>
</plist>
`
	if got != want {
		t.Errorf("plist mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}

	// Deterministic: map iteration order in text/template is sorted by key, so re-rendering with
	// the same input must byte-for-byte match (this is what makes Check's fileHasContent safe).
	got2, err := RenderLaunchAgent(LaunchAgentSpec{
		Label:                       "com.grove.orchard-worker",
		Program:                     "/Users/alice/.local/bin/orchard",
		Args:                        []string{"worker", "run", "--name", "alices-mac", "https://grove-cp.tailnet.ts.net:6120"},
		WorkingDir:                  "/Users/alice",
		Env:                         map[string]string{"B_VAR": "2", "A_VAR": "1"},
		AssociatedBundleIdentifiers: []string{"com.grove.orchard-worker"},
		StdoutPath:                  "/Users/alice/Library/Logs/grove/orchard-worker.log",
		StderrPath:                  "/Users/alice/Library/Logs/grove/orchard-worker.err.log",
		KeepAlive:                   true,
		RunAtLoad:                   true,
	})
	if err != nil {
		t.Fatalf("RenderLaunchAgent (2nd): %v", err)
	}
	if got != got2 {
		t.Error("RenderLaunchAgent is not deterministic across identical calls")
	}
}

func TestRenderSystemdUnit(t *testing.T) {
	got, err := RenderSystemdUnit(SystemdUnitSpec{
		Description: "Nomad server",
		Program:     "nomad",
		Args:        []string{"agent", "-config", "/home/bob/.config/grove/nomad/server.hcl"},
		WorkingDir:  "/home/bob/.config/grove/nomad/data",
		Env:         map[string]string{"NOMAD_ADDR": "http://100.64.0.5:4646"},
	})
	if err != nil {
		t.Fatalf("RenderSystemdUnit: %v", err)
	}
	want := "# Managed by grove install. Re-run `grove install` to regenerate; do not edit by hand.\n" +
		"[Unit]\n" +
		"Description=Nomad server\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=nomad agent -config /home/bob/.config/grove/nomad/server.hcl\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"WorkingDirectory=/home/bob/.config/grove/nomad/data\n" +
		"Environment=NOMAD_ADDR=http://100.64.0.5:4646\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
	if got != want {
		t.Errorf("systemd unit mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderNomadServerConfig(t *testing.T) {
	spec := NomadServerConfigSpec{
		DataDir:  "/home/bob/.config/grove/nomad/data",
		BindAddr: "100.64.0.5",
		TLS:      NomadTLSFiles{CA: "/tls/ca.pem", Cert: "/tls/server.pem", Key: "/tls/server-key.pem"},
	}
	got, err := RenderNomadServerConfig(spec)
	if err != nil {
		t.Fatalf("RenderNomadServerConfig: %v", err)
	}
	again, _ := RenderNomadServerConfig(spec)
	if got != again {
		t.Fatal("render is not deterministic")
	}
	for _, want := range []string{
		`data_dir  = "/home/bob/.config/grove/nomad/data"`,
		`bind_addr = "100.64.0.5"`,
		"  client_introduction {\n    enforcement = \"strict\"\n  }\n",
		"tls {\n  http                   = true\n  rpc                    = true\n" +
			"  ca_file                = \"/tls/ca.pem\"\n  cert_file              = \"/tls/server.pem\"\n" +
			"  key_file               = \"/tls/server-key.pem\"\n  verify_server_hostname = true\n" +
			"  verify_https_client    = true\n}\n",
		"acl {\n",
		"  enabled = true\n}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("server.hcl missing %q:\n%s", want, got)
		}
	}
	// The sandbox-host client is its own agent now: the server runs no client, and so no jobs.
	for _, unwanted := range []string{"client {", "raw_exec", "grove_sandbox_host"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("server.hcl must not contain %q:\n%s", unwanted, got)
		}
	}
}

func TestRenderNomadSandboxClientConfig(t *testing.T) {
	got, err := RenderNomadSandboxClientConfig(NomadSandboxClientConfigSpec{
		DataDir: "/home/bob/.config/grove/nomad/sandbox-data",
		Server:  "100.64.0.5:4647",
		TLS:     NomadTLSFiles{CA: "/tls/ca.pem", Cert: "/tls/node.pem", Key: "/tls/node-key.pem"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`name      = "grove-sandbox-host"`,
		`data_dir  = "/home/bob/.config/grove/nomad/sandbox-data"`,
		`bind_addr = "127.0.0.1"`,
		"  http = 4656\n  rpc  = 4657\n  serf = 4658\n",
		`  servers = ["100.64.0.5:4647"]`,
		`    grove_sandbox_host = "true"`,
		"tls {\n  rpc                    = true\n  ca_file                = \"/tls/ca.pem\"\n" +
			"  cert_file              = \"/tls/node.pem\"\n  key_file               = \"/tls/node-key.pem\"\n" +
			"  verify_server_hostname = true\n}\n",
		"acl {\n  enabled = true\n}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sandbox-client.hcl missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "server {") || strings.Contains(got, "pool =") {
		t.Errorf("the sandbox-host client must not be a server or join a pool:\n%s", got)
	}
}

func TestRenderOrchardControllerEnv(t *testing.T) {
	got, err := RenderOrchardControllerEnv(OrchardControllerEnvSpec{
		Home:    "/home/bob/.config/grove/orchard-controller/data",
		Address: "100.64.0.5:6120",
	})
	if err != nil {
		t.Fatalf("RenderOrchardControllerEnv: %v", err)
	}
	want := "# Managed by grove install. Re-run `grove install --role control-plane` to regenerate.\n" +
		"ORCHARD_HOME=/home/bob/.config/grove/orchard-controller/data\n" +
		"ORCHARD_ADDRESS=100.64.0.5:6120\n"
	if got != want {
		t.Errorf("orchard controller env mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}
