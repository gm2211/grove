package install

import "testing"

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
		Description: "Orchard controller",
		Program:     "/home/bob/.local/bin/orchard",
		Args:        []string{"controller", "run"},
		WorkingDir:  "/home/bob/.config/grove/orchard-controller",
		Env:         map[string]string{"ORCHARD_HOME": "/home/bob/.config/grove/orchard-controller"},
	})
	if err != nil {
		t.Fatalf("RenderSystemdUnit: %v", err)
	}
	want := "# Managed by grove install. Re-run `grove install` to regenerate; do not edit by hand.\n" +
		"[Unit]\n" +
		"Description=Orchard controller\n" +
		"After=network-online.target\n" +
		"Wants=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"ExecStart=/home/bob/.local/bin/orchard controller run\n" +
		"Restart=always\n" +
		"RestartSec=5\n" +
		"WorkingDirectory=/home/bob/.config/grove/orchard-controller\n" +
		"Environment=ORCHARD_HOME=/home/bob/.config/grove/orchard-controller\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=default.target\n"
	if got != want {
		t.Errorf("systemd unit mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
