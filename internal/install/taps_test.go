package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

func TestTrustTap_RefusesWhenHomebrewBelongsToAnotherUser(t *testing.T) {
	r := NewFakeRunner()
	r.Script("brew --prefix", FakeResult{Stdout: "/opt/homebrew\n"})
	r.Script("stat -f %Su /opt/homebrew", FakeResult{Stdout: "someone-else-entirely\n"})

	err := trustTap(context.Background(), r, tapOpenAITools, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected an error when the Homebrew prefix belongs to another user")
	}
	for _, want := range []string{"/opt/homebrew", "someone-else-entirely", "brew tap openai/tools && brew trust openai/tools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if r.CalledWith("brew tap openai/tools") || r.CalledWith("brew trust openai/tools") {
		t.Errorf("brew tap/trust must not run on a Homebrew this user doesn't own: %v", r.Calls)
	}
}

func TestTrustTap_ProceedsWhenPrefixOwnerUnknown(t *testing.T) {
	r := NewFakeRunner() // brew --prefix prints nothing: the owner can't be determined
	if err := trustTap(context.Background(), r, tapOpenAITools, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !r.CalledWith("brew tap openai/tools") || !r.CalledWith("brew trust openai/tools") {
		t.Errorf("expected brew tap and trust to run, calls: %v", r.Calls)
	}
}

func TestTrustTapStep_CheckQueriesWithoutRefreshingHomebrew(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, true)
	ok, err := trustTapStep(r, tapOpenAITools, io.Discard).Check(context.Background())
	if err != nil || !ok {
		t.Fatalf("Check = %v, %v; want satisfied", ok, err)
	}
	if len(r.Calls) != 1 || r.Calls[0].Name != "env" {
		t.Errorf("calls = %v, want one `env HOMEBREW_NO_AUTO_UPDATE=1 ... brew tap-info`", r.Calls)
	}
}

// scriptTapInfo scripts `brew tap-info --json=v1 <tap>` on r the way the real Homebrew command
// renders it: a one-element JSON array (see the "installed"/"trusted" fields taps.go reads).
func scriptTapInfo(r *FakeRunner, tap string, installed, trusted bool) {
	r.Script(fmt.Sprintf("env HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1 brew tap-info --json=v1 %s", tap), FakeResult{
		Stdout: fmt.Sprintf(`[{"name":%q,"installed":%t,"trusted":%t}]`, tap, installed, trusted),
	})
}

func TestFetchTapInfo_ParsesInstalledAndTrusted(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, false)

	info, err := fetchTapInfo(context.Background(), r, "openai/tools")
	if err != nil {
		t.Fatalf("fetchTapInfo: %v", err)
	}
	if !info.Installed || info.Trusted {
		t.Errorf("fetchTapInfo = %+v, want installed=true trusted=false", info)
	}
}

func TestFetchTapInfo_ErrorsOnRunnerFailure(t *testing.T) {
	r := NewFakeRunner()
	r.Script("env HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1 brew tap-info --json=v1 openai/tools", FakeResult{Err: errors.New("brew: command not found (fake)")})

	if _, err := fetchTapInfo(context.Background(), r, "openai/tools"); err == nil {
		t.Error("expected fetchTapInfo to surface the runner error")
	}
}

func TestFetchTapInfo_ErrorsOnUnparsableOutput(t *testing.T) {
	r := NewFakeRunner()
	r.Script("env HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1 brew tap-info --json=v1 openai/tools", FakeResult{Stdout: "not json"})

	if _, err := fetchTapInfo(context.Background(), r, "openai/tools"); err == nil {
		t.Error("expected fetchTapInfo to error on unparsable output")
	}
}

func TestIsUnknownBrewCommand(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("brew trust openai/tools: exit status 1: Error: Invalid usage: Unknown command: brew trust openai/tools"), true},
		{errors.New("exit status 1: some other failure"), false},
	}
	for _, c := range cases {
		if got := isUnknownBrewCommand(c.err); got != c.want {
			t.Errorf("isUnknownBrewCommand(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestTrustTap_TapsThenTrustsAndExplains(t *testing.T) {
	r := NewFakeRunner()
	var out bytes.Buffer

	if err := trustTap(context.Background(), r, tapOpenAITools, &out); err != nil {
		t.Fatalf("trustTap: %v", err)
	}

	tapIdx, trustIdx := -1, -1
	for i, c := range r.Calls {
		switch c.String() {
		case "brew tap openai/tools":
			tapIdx = i
		case "brew trust openai/tools":
			trustIdx = i
		}
	}
	if tapIdx == -1 {
		t.Error("expected `brew tap openai/tools` to run")
	}
	if trustIdx == -1 {
		t.Error("expected `brew trust openai/tools` to run")
	}
	if tapIdx != -1 && trustIdx != -1 && tapIdx > trustIdx {
		t.Errorf("expected `brew tap` (call %d) to run before `brew trust` (call %d)", tapIdx, trustIdx)
	}
	if !bytes.Contains(out.Bytes(), []byte("openai/tools")) {
		t.Errorf("expected an explanation mentioning openai/tools to be printed, got %q", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("Tart")) {
		t.Errorf("expected the explanation to say what the tap is for, got %q", out.String())
	}
}

func TestTrustTap_TapFailureIsFatal(t *testing.T) {
	r := NewFakeRunner()
	r.Script("brew tap openai/tools", FakeResult{Err: errors.New("network unreachable (fake)")})

	err := trustTap(context.Background(), r, tapOpenAITools, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected trustTap to fail when `brew tap` fails")
	}
	if r.CalledWith("brew trust openai/tools") {
		t.Error("`brew trust` should not run when `brew tap` already failed")
	}
}

func TestTrustTap_ToleratesUnknownTrustCommand(t *testing.T) {
	r := NewFakeRunner()
	r.Script("brew trust openai/tools", FakeResult{
		Err: errors.New("brew trust openai/tools: exit status 1: Error: Invalid usage: Unknown command: brew trust openai/tools"),
	})

	if err := trustTap(context.Background(), r, tapOpenAITools, &bytes.Buffer{}); err != nil {
		t.Errorf("expected an unknown `brew trust` command to be tolerated (older Homebrew), got: %v", err)
	}
}

func TestTrustTap_OtherTrustFailuresPropagate(t *testing.T) {
	r := NewFakeRunner()
	r.Script("brew trust openai/tools", FakeResult{Err: errors.New("some real failure (fake)")})

	if err := trustTap(context.Background(), r, tapOpenAITools, &bytes.Buffer{}); err == nil {
		t.Error("expected a genuine `brew trust` failure to propagate")
	}
}

func TestTrustTapStep_CheckSatisfiedWhenInstalledAndTrusted(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, true)

	step := trustTapStep(r, tapOpenAITools, &bytes.Buffer{})
	ok, err := step.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !ok {
		t.Error("expected Check to report satisfied when the tap is installed and trusted")
	}
}

func TestTrustTapStep_CheckNotSatisfiedWhenUntrusted(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, false)

	step := trustTapStep(r, tapOpenAITools, &bytes.Buffer{})
	ok, err := step.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if ok {
		t.Error("expected Check to report not-satisfied for an installed-but-untrusted tap")
	}
}

func TestTrustTapStep_CheckNotSatisfiedWhenTapInfoFails(t *testing.T) {
	r := NewFakeRunner()
	r.Script("env HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_FROM_API=1 brew tap-info --json=v1 openai/tools", FakeResult{Err: errors.New("brew: command not found (fake)")})

	step := trustTapStep(r, tapOpenAITools, &bytes.Buffer{})
	ok, err := step.Check(context.Background())
	if err != nil {
		t.Fatalf("Check should swallow tap-info failures (to give Apply a chance to self-heal), got error: %v", err)
	}
	if ok {
		t.Error("expected Check to report not-satisfied when tap-info can't be read")
	}
}

func TestCheckTrustedTaps_FlagsInstalledButUntrusted(t *testing.T) {
	r := NewFakeRunner()
	scriptTapInfo(r, "openai/tools", true, false)
	scriptTapInfo(r, "cirruslabs/cli", false, false)
	scriptTapInfo(r, "hashicorp/tap", true, true)
	// grove no longer installs MinIO: even tapped and untrusted, minio/stable isn't audited.
	scriptTapInfo(r, "minio/stable", true, false)

	opts := Options{GOOS: "darwin"}
	results := checkTrustedTaps(context.Background(), r, opts)

	byName := map[string]CheckResult{}
	for _, res := range results {
		byName[res.Name] = res
	}

	openai, ok := byName["brew-trust:openai/tools"]
	if !ok {
		t.Fatal("expected a result for the installed-but-untrusted openai/tools tap")
	}
	if openai.OK {
		t.Error("expected openai/tools to be flagged as not OK")
	}
	if openai.Remediation != "brew trust openai/tools" {
		t.Errorf("Remediation = %q, want the exact `brew trust openai/tools` command", openai.Remediation)
	}

	if _, ok := byName["brew-trust:cirruslabs/cli"]; ok {
		t.Error("a tap that was never tapped should not be reported (it isn't required yet)")
	}
	if _, ok := byName["brew-trust:minio/stable"]; ok {
		t.Error("minio/stable is no longer a grove dependency and should not be audited")
	}

	hashicorp, ok := byName["brew-trust:hashicorp/tap"]
	if !ok {
		t.Fatal("expected a result for the installed-and-trusted hashicorp/tap")
	}
	if !hashicorp.OK {
		t.Error("expected hashicorp/tap to report OK since it's already trusted")
	}
}

func TestCheckTrustedTaps_NoOpOnNonDarwin(t *testing.T) {
	r := NewFakeRunner()
	opts := Options{GOOS: "linux"}
	if results := checkTrustedTaps(context.Background(), r, opts); results != nil {
		t.Errorf("expected no results on non-darwin, got %+v", results)
	}
	if len(r.Calls) != 0 {
		t.Errorf("expected no brew commands to run on non-darwin, got %+v", r.Calls)
	}
}
