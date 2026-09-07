package install

import "testing"

func TestRenderLaunchAgent(t *testing.T) {
	got, err := RenderLaunchAgent(LaunchAgentSpec{
		Label:      "com.grove.orchard-worker",
		Program:    "/Users/alice/.local/bin/orchard",
		Args:       []string{"worker", "run", "--name", "alices-mac", "https://grove-cp.tailnet.ts.net:6120"},
		WorkingDir: "/Users/alice",
		Env:        map[string]string{"B_VAR": "2", "A_VAR": "1"},
		StdoutPath: "/Users/alice/Library/Logs/grove/orchard-worker.log",
		StderrPath: "/Users/alice/Library/Logs/grove/orchard-worker.err.log",
		KeepAlive:  true,
		RunAtLoad:  true,
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
		Label:      "com.grove.orchard-worker",
		Program:    "/Users/alice/.local/bin/orchard",
		Args:       []string{"worker", "run", "--name", "alices-mac", "https://grove-cp.tailnet.ts.net:6120"},
		WorkingDir: "/Users/alice",
		Env:        map[string]string{"B_VAR": "2", "A_VAR": "1"},
		StdoutPath: "/Users/alice/Library/Logs/grove/orchard-worker.log",
		StderrPath: "/Users/alice/Library/Logs/grove/orchard-worker.err.log",
		KeepAlive:  true,
		RunAtLoad:  true,
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
	got, err := RenderNomadServerConfig(NomadServerConfigSpec{
		DataDir:  "/home/bob/.config/grove/nomad/data",
		BindAddr: "100.64.0.5",
	})
	if err != nil {
		t.Fatalf("RenderNomadServerConfig: %v", err)
	}
	want := "# Managed by grove install. Re-run `grove install --role control-plane` to regenerate.\n" +
		`data_dir  = "/home/bob/.config/grove/nomad/data"` + "\n" +
		`bind_addr = "100.64.0.5"` + "\n" +
		"\n" +
		"server {\n" +
		"  enabled          = true\n" +
		"  bootstrap_expect = 1\n" +
		"}\n" +
		"\n" +
		"acl {\n" +
		"  # Off: the control plane is reachable only over the tailnet, nothing routes here from outside.\n" +
		"  enabled = false\n" +
		"}\n"
	if got != want {
		t.Errorf("nomad config mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderMinIOEnv(t *testing.T) {
	got, err := RenderMinIOEnv(MinIOEnvSpec{
		RootUser:     "abc123",
		RootPassword: "secretsecret",
		DataDir:      "/home/bob/.config/grove/minio/data",
		Address:      "100.64.0.5:9000",
	})
	if err != nil {
		t.Fatalf("RenderMinIOEnv: %v", err)
	}
	want := "# Managed by grove install. Re-run `grove install --role control-plane` to regenerate.\n" +
		"MINIO_ROOT_USER=abc123\n" +
		"MINIO_ROOT_PASSWORD=secretsecret\n" +
		"MINIO_VOLUMES=/home/bob/.config/grove/minio/data\n" +
		"MINIO_ADDRESS=100.64.0.5:9000\n"
	if got != want {
		t.Errorf("minio env mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
