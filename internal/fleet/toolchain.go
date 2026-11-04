package fleet

import (
	"strconv"
	"strings"
)

// Native toolchains on shared macOS pools.
//
// A job's environment may carry a typed toolchain spec (rust, node, python at exact versions). The
// control plane validates it and hands the job its canonical JSON and hash as the dispatch meta
// toolchain_json and toolchain_hash. A macOS guest installs the spec once, with mise, into a
// content-addressed directory under MacOSToolchainRoot that the job user can run but not write,
// then every job with that spec reuses it. The install runs as its own unprivileged user,
// MacOSToolchainUser, in the "source" task after the clone token is gone; the job user only ever
// gets the finished directory on PATH (nomad/jobs/build.nomad.hcl).

const (
	// MacOSToolchainUser owns MacOSToolchainRoot and runs the installer: no shell, no home, no
	// password, fenced off home networks like the job user. It is the only account that runs mise.
	MacOSToolchainUser = "_grovetool"
	// MacOSToolchainRoot holds one directory per (mise version, spec hash). Owned by
	// MacOSToolchainUser, mode 0755: jobs read and execute, never write.
	MacOSToolchainRoot = "/private/var/grove-toolchains"
	// MacOSToolchainInstaller is the root-owned installer the source task runs as MacOSToolchainUser.
	MacOSToolchainInstaller = "/usr/local/libexec/grove-toolchain"
	// MacOSMise is where the image bakes the pinned mise binary.
	MacOSMise = "/usr/local/bin/mise"
	// MacOSToolchainUseDir is the sticky, world-writable directory where run.sh drops a marker file
	// per job using a toolchain entry, which the installer's eviction reads.
	MacOSToolchainUseDir = MacOSToolchainRoot + "/.use"

	// ToolchainHashPrefix is hashed ahead of the canonical JSON: hash = sha256(prefix + json).
	ToolchainHashPrefix = "grove-toolchain/1\n"
	// ToolchainMaxBytes is the largest spec the control plane sends and the installer accepts.
	ToolchainMaxBytes = 1024
)

// The exact-version patterns, shared by the control plane's validation and the installer's own
// re-validation. [0-9] rather than \d: jq's regex engine would accept other scripts' digits for \d.
const (
	toolchainRustPattern   = `^1\.[0-9]+\.[0-9]+$`
	toolchainNodePattern   = `^[0-9]+\.[0-9]+\.[0-9]+$`
	toolchainPythonPattern = `^3\.[0-9]+\.[0-9]+$`
)

// toolchainComponents is the closed set of rustup components a spec may name.
var toolchainComponents = []string{"clippy", "rustfmt", "rust-src", "llvm-tools"}

// A cache entry unused this long may be evicted; the cache keeps up to toolchainKeepEntries
// regardless.
const (
	toolchainKeepEntries   = 3
	toolchainIdleMinutes   = 24 * 60
	toolchainStaleLockMins = 30
	toolchainInstallSecs   = 20 * 60
)

// macosToolchainUserScript creates MacOSToolchainUser and the cache directory, and installs the
// root-owned installer. Idempotent: it runs on every boot.
var macosToolchainUserScript = `if ! dscl . -read /Groups/` + MacOSToolchainUser + ` >/dev/null 2>&1; then
  grove_priv dscl . -create /Groups/` + MacOSToolchainUser + `
  grove_priv dscl . -create /Groups/` + MacOSToolchainUser + ` PrimaryGroupID 7462
  grove_priv dscl . -create /Groups/` + MacOSToolchainUser + ` Password '*'
fi
if ! dscl . -read /Users/` + MacOSToolchainUser + ` >/dev/null 2>&1; then
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + `
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` UniqueID 7462
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` PrimaryGroupID 7462
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` UserShell /usr/bin/false
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` NFSHomeDirectory /var/empty
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` RealName "grove toolchain"
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` Password '*'
  grove_priv dscl . -create /Users/` + MacOSToolchainUser + ` IsHidden 1
fi
grove_priv mkdir -p ` + MacOSToolchainRoot + ` ` + MacOSToolchainUseDir + ` /usr/local/libexec
grove_priv chown ` + MacOSToolchainUser + `:` + MacOSToolchainUser + ` ` + MacOSToolchainRoot + ` ` + MacOSToolchainUseDir + `
grove_priv chmod 0755 ` + MacOSToolchainRoot + `
grove_priv chmod 1777 ` + MacOSToolchainUseDir + `
grove_priv tee ` + MacOSToolchainInstaller + ` >/dev/null <<'GROVE_TOOLCHAIN_INSTALLER'
` + strings.TrimSuffix(macosToolchainInstaller, "\n") + `
GROVE_TOOLCHAIN_INSTALLER
grove_priv chown root:wheel ` + MacOSToolchainInstaller + `
grove_priv chmod 0755 ` + MacOSToolchainInstaller + `
`

// toolchainJQValidation is the jq program that re-checks a spec: the same shape and patterns the
// control plane enforces. jq -e fails (exit 1) on false, and on any error.
func toolchainJQValidation() string {
	esc := func(p string) string { return strings.ReplaceAll(p, `\`, `\\`) }
	comps := make([]string, len(toolchainComponents))
	for i, c := range toolchainComponents {
		comps[i] = `. == "` + c + `"`
	}
	return strings.NewReplacer(
		"@RUST@", esc(toolchainRustPattern),
		"@NODE@", esc(toolchainNodePattern),
		"@PYTHON@", esc(toolchainPythonPattern),
		"@COMPONENTS@", strings.Join(comps, " or "),
	).Replace(`def ok(p): type == "string" and test(p) and (test("[\\n\\r]") | not);
def only(k): (keys - k) == [];
type == "object" and length > 0 and only(["rust", "node", "python"])
and ((has("rust") | not) or (.rust | type == "object" and only(["version", "components"])
  and (.version | ok("@RUST@"))
  and ((has("components") | not) or (.components | type == "array" and all(.[]; @COMPONENTS@)))))
and ((has("node") | not) or (.node | type == "object" and only(["version"]) and (.version | ok("@NODE@"))))
and ((has("python") | not) or (.python | type == "object" and only(["version"]) and (.version | ok("@PYTHON@"))))`)
}

// macosToolchainInstaller is /usr/local/libexec/grove-toolchain. It runs as MacOSToolchainUser (the
// source task starts it with sudo -u), gets the spec on stdin and the expected hash as its one
// argument, prints the finished directory on stdout and everything else on stderr. It trusts
// nothing it is given: the spec is re-validated and re-hashed, and mise only ever sees a
// mise.toml this script wrote ([tools] only), from a directory that is not the checkout.
var macosToolchainInstaller = strings.NewReplacer(
	"@MISE@", MacOSMise,
	"@ROOT@", MacOSToolchainRoot,
	"@MAX_BYTES@", strconv.Itoa(ToolchainMaxBytes),
	"@STALE_LOCK@", strconv.Itoa(toolchainStaleLockMins),
	"@INSTALL_SECS@", strconv.Itoa(toolchainInstallSecs),
	"@KEEP@", strconv.Itoa(toolchainKeepEntries),
	"@IDLE_MINS@", strconv.Itoa(toolchainIdleMinutes),
	"@JQ_VALIDATION@", toolchainJQValidation(),
).Replace(macosToolchainInstallerTemplate)

const macosToolchainInstallerTemplate = `#!/bin/bash
# Installed by grove's fleet startup script for macOS guests. Runs as the toolchain user, never as
# root and never in a job's checkout: grove-toolchain <sha256 of the spec> < spec.json
set -eu
umask 022
LC_ALL=C
export LC_ALL
PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin
export PATH

grove_root=@ROOT@
mise_bin=@MISE@

fail() { echo "grove-toolchain: $*" >&2; exit 2; }

want_hash="${1:-}"
case "$want_hash" in
  "" | *[!0-9a-f]*) fail "usage: grove-toolchain <sha256 hex of the spec> < spec.json" ;;
esac
[ "${#want_hash}" -eq 64 ] || fail "the hash must be 64 hex digits"

spec="$(head -c $((@MAX_BYTES@ + 1)))"
[ -n "$spec" ] || fail "empty spec"
[ "${#spec}" -le @MAX_BYTES@ ] || fail "spec is larger than @MAX_BYTES@ bytes"

sha256() {
  if command -v shasum >/dev/null 2>&1; then shasum -a 256; else sha256sum; fi | awk '{ print $1 }'
}
got_hash="$(printf 'grove-toolchain/1\n%s' "$spec" | sha256)"
[ "$got_hash" = "$want_hash" ] || fail "the spec does not match its hash"

grove_jq_validation='@JQ_VALIDATION@'
printf '%s' "$spec" | jq -e "$grove_jq_validation" >/dev/null 2>&1 || fail "the spec is not a valid toolchain spec"

rust_v="$(printf '%s' "$spec" | jq -r '.rust.version // empty')"
rust_c="$(printf '%s' "$spec" | jq -r '(.rust.components // []) | join(",")')"
node_v="$(printf '%s' "$spec" | jq -r '.node.version // empty')"
py_v="$(printf '%s' "$spec" | jq -r '.python.version // empty')"
for v in "$rust_v" "$node_v" "$py_v"; do
  case "$v" in *[!0-9.]*) fail "bad version" ;; esac
done
case "$rust_c" in *[!a-z,-]*) fail "bad component" ;; esac

mise_version="$("$mise_bin" --version | awk '{ print $1; exit }')"
case "$mise_version" in
  "" | *[!0-9A-Za-z.+-]*) fail "cannot read mise's version" ;;
esac
name="mise-$mise_version-$want_hash"
dir="$grove_root/$name"
use_dir="$grove_root/.use"

mtime() { stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0; }

# Entries nobody has used for @IDLE_MINS@ minutes go, oldest first, until at most @KEEP@ remain. A
# job in use keeps its entry: run.sh drops a marker in the use directory, and an entry whose
# marker or last-use stamp is newer than that is never evicted. Best effort.
evict() {
  mkdir -p "$use_dir" 2>/dev/null || true
  find "$use_dir" -type f -mmin +@IDLE_MINS@ -delete 2>/dev/null || true
  count=0
  list=""
  for entry in "$grove_root"/mise-*-*; do
    [ -f "$entry/.complete" ] || continue
    last="$(mtime "$entry/.lastuse")"
    for marker in "$use_dir/$(basename "$entry")".*; do
      [ -e "$marker" ] || continue
      m="$(mtime "$marker")"
      [ "$m" -le "$last" ] || last="$m"
    done
    count=$((count + 1))
    list="$list$last $entry
"
  done
  now="$(date +%s)"
  printf '%s' "$list" | sort -n | while read -r last entry; do
    [ "$count" -gt @KEEP@ ] || break
    [ "$entry" != "$dir" ] || continue
    [ $((now - last)) -gt $((@IDLE_MINS@ * 60)) ] || continue
    case "$entry" in "$grove_root"/mise-*-????????????????????????????????????????????????????????????????) ;; *) continue ;; esac
    rm -rf "$entry" "$use_dir/$(basename "$entry")".* 2>/dev/null || true
    count=$((count - 1))
  done
}

if [ -f "$dir/.complete" ]; then
  touch "$dir/.lastuse" 2>/dev/null || true
  evict || true
  printf '%s\n' "$dir"
  exit 0
fi

# One installer per entry. A lock older than @STALE_LOCK@ minutes belongs to a dead installer.
lock="$grove_root/.lock-$name"
waited=0
until mkdir "$lock" 2>/dev/null; do
  if [ -n "$(find "$lock" -maxdepth 0 -mmin +@STALE_LOCK@ 2>/dev/null)" ]; then
    rm -rf "$lock"
    continue
  fi
  if [ -f "$dir/.complete" ]; then
    touch "$dir/.lastuse" 2>/dev/null || true
    printf '%s\n' "$dir"
    exit 0
  fi
  [ "$waited" -lt $((@INSTALL_SECS@ + 60)) ] || fail "another install of this toolchain did not finish"
  sleep 5
  waited=$((waited + 5))
done
trap 'rm -rf "$lock"' EXIT
if [ -f "$dir/.complete" ]; then
  touch "$dir/.lastuse" 2>/dev/null || true
  printf '%s\n' "$dir"
  exit 0
fi

# A half-finished earlier attempt is thrown away; the directory is ours alone while we hold the lock.
rm -rf "$dir"
mkdir "$dir" "$dir/tmp" "$dir/home"

# The one config mise reads is the one written here: tools only, nothing from the spec but its
# validated versions and components.
{
  echo "[tools]"
  if [ -n "$rust_v" ]; then
    if [ -n "$rust_c" ]; then
      echo "rust = { version = \"$rust_v\", profile = \"minimal\", components = \"$rust_c\" }"
    else
      echo "rust = { version = \"$rust_v\", profile = \"minimal\" }"
    fi
  fi
  [ -z "$node_v" ] || echo "node = \"$node_v\""
  [ -z "$py_v" ] || echo "python = \"$py_v\""
} > "$dir/mise.toml"

cd "$dir"

run_mise() {
  env -i PATH="$PATH" HOME=/var/empty LC_ALL=C TMPDIR="$dir/tmp" \
    MISE_DATA_DIR="$dir/data" MISE_CACHE_DIR="$dir/cache" MISE_STATE_DIR="$dir/state" \
    MISE_GLOBAL_CONFIG_FILE=/dev/null MISE_SYSTEM_CONFIG_FILE=/dev/null \
    MISE_TRUSTED_CONFIG_PATHS="$dir" MISE_PYTHON_COMPILE=0 \
    RUSTUP_HOME="$dir/rustup" CARGO_HOME="$dir/cargo" MISE_YES=1 \
    "$mise_bin" "$@"
}

# The install gets @INSTALL_SECS@ seconds, then TERM, then KILL, for the whole process group.
set -m
run_mise install >&2 &
pid=$!
set +m
marker="$dir/tmp/timed-out"
(
  sleep @INSTALL_SECS@
  : > "$marker"
  kill -TERM "-$pid" 2>/dev/null
  sleep 10
  kill -KILL "-$pid" 2>/dev/null
) &
watchdog=$!
code=0
wait "$pid" || code=$?
pkill -P "$watchdog" 2>/dev/null || true
kill "$watchdog" 2>/dev/null || true
wait "$watchdog" 2>/dev/null || true
if [ -f "$marker" ]; then
  rm -rf "$dir"
  fail "mise install took longer than @INSTALL_SECS@ seconds"
fi
if [ "$code" -ne 0 ]; then
  rm -rf "$dir"
  fail "mise install failed ($code)"
fi

# A rust toolchain must be rustup's default, since jobs run without RUSTUP_TOOLCHAIN.
if [ -n "$rust_v" ]; then
  run_mise exec -- rustup default "$rust_v" >&2 || { rm -rf "$dir"; fail "rustup default failed"; }
fi

run_mise bin-paths > "$dir/paths.tmp" || { rm -rf "$dir"; fail "mise bin-paths failed"; }
while IFS= read -r p; do
  case "$p" in
    "$dir"/*) ;;
    *) rm -rf "$dir"; fail "mise reported a path outside the toolchain directory" ;;
  esac
done < "$dir/paths.tmp"
mv "$dir/paths.tmp" "$dir/paths"

touch "$dir/.lastuse"
touch "$dir/.complete"
evict || true
printf '%s\n' "$dir"
`
