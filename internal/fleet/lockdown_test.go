package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// macOS's own /etc/pam.d/su and /etc/pam.d/login, as the lockdown finds them.
const (
	macosPAMSu = `# su: auth account session
auth       sufficient     pam_rootok.so
auth       required       pam_opendirectory.so
account    required       pam_group.so no_warn group=admin,wheel ruser root_only fail_safe
account    required       pam_opendirectory.so no_check_shell
password   required       pam_opendirectory.so
session    required       pam_launchd.so
`
	macosPAMLogin = `# login: auth account password session
auth       optional       pam_krb5.so use_kcminit
auth       optional       pam_ntlm.so try_first_pass
auth       optional       pam_mount.so try_first_pass
auth       required       pam_opendirectory.so try_first_pass
account    required       pam_nologin.so
account    required       pam_opendirectory.so
password   required       pam_opendirectory.so
session    required       pam_launchd.so
session    optional       pam_mount.so
`
)

// macosPAMGate is the line the lockdown adds to su and login.
const macosPAMGate = "auth       required       pam_group.so no_warn group=admin ruser # grove\n"

// fakeMacOSLockdown points a macOS startup script's lockdown at PAM files in a temp dir, never the
// test machine's own, and puts a `security` stub in fakeBin that logs its arguments to the
// returned path and exits with $GROVE_TEST_SECURITY_EXIT (0 when unset). gated starts the files
// already gated, for tests whose fake sudo runs nothing.
func fakeMacOSLockdown(t *testing.T, fakeBin, script string, gated bool) (rewritten, pamDir, securityLog string) {
	t.Helper()
	pamDir = filepath.Join(t.TempDir(), "pam.d")
	if err := os.MkdirAll(pamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"su": macosPAMSu, "login": macosPAMLogin} {
		if gated {
			body = macosPAMGate + body
		}
		if err := os.WriteFile(filepath.Join(pamDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	securityLog = filepath.Join(t.TempDir(), "security.log")
	stub := "#!/bin/sh\necho \"$*\" >> " + shQuote(securityLog) + "\nexit \"${GROVE_TEST_SECURITY_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "security"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "/etc/pam.d/su") {
		t.Fatal("script has no lockdown to point elsewhere")
	}
	return strings.ReplaceAll(script, "/etc/pam.d", pamDir), pamDir, securityLog
}

// The lockdown gates su and login to callers already in admin, ahead of every other auth module
// (after pam_rootok, so root still su's), denies system.privilege.admin, changes nothing on a
// second run, and stops the script when a step fails.
func TestMacOSLockdownScript(t *testing.T) {
	fakeBin := t.TempDir()
	script, pamDir, securityLog := fakeMacOSLockdown(t, fakeBin, "set -eu\ngrove_priv() { \"$@\"; }\n"+macosLockdownScript+"echo locked-down\n", false)
	run := func(env ...string) (string, error) {
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Env = append(append(os.Environ(), "PATH="+fakeBin+":"+os.Getenv("PATH")), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	gate := macosPAMGate
	want := map[string]string{
		"su":    strings.Replace(macosPAMSu, "auth       required       pam_opendirectory.so\n", gate+"auth       required       pam_opendirectory.so\n", 1),
		"login": strings.Replace(macosPAMLogin, "auth       optional       pam_krb5.so", strings.TrimSuffix(gate, "\n")+"\nauth       optional       pam_krb5.so", 1),
	}
	for i := 0; i < 2; i++ {
		if out, err := run(); err != nil || !strings.Contains(out, "locked-down") {
			t.Fatalf("run %d: %v\n%s", i, err, out)
		}
		for name, body := range want {
			got, err := os.ReadFile(filepath.Join(pamDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != body {
				t.Errorf("run %d: %s =\n%s\nwant:\n%s", i, name, got, body)
			}
		}
	}
	log, _ := os.ReadFile(securityLog)
	if string(log) != "authorizationdb write system.privilege.admin deny\nauthorizationdb write system.privilege.admin deny\n" {
		t.Errorf("security calls = %q", log)
	}

	if out, err := run("GROVE_TEST_SECURITY_EXIT=1"); err == nil || strings.Contains(out, "locked-down") || !strings.Contains(out, "system.privilege.admin") {
		t.Errorf("a failed authorizationdb write must stop the script: %v\n%s", err, out)
	}
}

// Every macOS guest, shared or sealed, locks down before Nomad (or the sealed job runner) can
// start a job, and keeps its job users off SSH; Linux guests have no admin to protect this way.
func TestBuildStartupScript_MacOSLocksDownBeforeJobs(t *testing.T) {
	for _, pool := range []Pool{{Name: "macos"}, {Name: "macos", Network: NetworkIsolated}} {
		script := buildStartupScript(pool, "mac1", "macos-mac1-0", Options{})
		lockAt := strings.Index(script, "# --- the job user can't become admin ---")
		userAt := strings.Index(script, "dscl . -create /Users/"+MacOSJobUser+" UniqueID")
		if lockAt < 0 || userAt < 0 || lockAt < userAt {
			t.Errorf("%s: lockdown missing or before the job user exists", pool.Network)
		}
		if !pool.Isolated() {
			if restartAt := strings.LastIndex(script, "launchctl kickstart -k system/com.grove.nomad"); restartAt < lockAt {
				t.Errorf("shared: lockdown after Nomad restarts")
			}
		}
		ssh := "block return out quick proto tcp from any to any port 22 user { " + MacOSJobUser
		if !strings.Contains(script, ssh) {
			t.Errorf("%s: job user not kept off SSH:\n%s", pool.Network, script)
		}
	}
	if sealed := buildStartupScript(Pool{Name: "macos", Network: NetworkIsolated}, "mac1", "macos-mac1-0", Options{}); strings.Contains(sealed, "port "+"3128") {
		t.Error("a sealed macOS guest gets no broker exception")
	}
	if strings.Contains(buildStartupScript(Pool{Name: "linux"}, "mac1", "linux-mac1-0", Options{}), "pam_group") {
		t.Error("Linux guests get no macOS lockdown")
	}
}
