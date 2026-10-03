#!/bin/sh
# grove bootstrap installer.
#
#   curl -fsSL https://raw.githubusercontent.com/gm2211/grove/main/scripts/install.sh | sh
#
# Installs the `grove` binary only. It does NOT run `grove install` for you — that command
# touches system settings (Homebrew, launchd, sudo) and needs a --role, so it's printed at the
# end for you (or Claude Code) to run explicitly.
#
# POSIX sh, safe under `curl | sh`: everything happens inside main(), invoked at the very end, so
# a truncated download (partial pipe) can't execute a half-written script.
set -eu

log() { printf '==> %s\n' "$1" >&2; }
die() {
	printf 'error: %s\n' "$1" >&2
	exit 1
}

detect_os() {
	case "$(uname -s)" in
	Darwin) echo darwin ;;
	Linux) echo linux ;;
	*) die "unsupported OS: $(uname -s) (grove supports macOS and Linux)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	arm64 | aarch64) echo arm64 ;;
	x86_64 | amd64) echo amd64 ;;
	*) die "unsupported architecture: $(uname -m)" ;;
	esac
}

have() { command -v "$1" >/dev/null 2>&1; }

# Homebrew's installer, pinned to a commit of github.com/Homebrew/install (it publishes no tags)
# and verified before it runs. Keep in sync with internal/install/download.go.
HOMEBREW_INSTALL_COMMIT=35da6871c4be7d7fdab2fd505fb7fa667926a2a5
HOMEBREW_INSTALL_SHA256=5f333bbe53bc490e51e7ccb1df8779b3dd6ee73a1a7379efda216edb08ccb148

# sha256_of FILE prints FILE's lower-case hex SHA-256.
sha256_of() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		die "need sha256sum or shasum to verify downloads"
	fi
}

# verify_sha256 FILE EXPECTED fails (and deletes FILE) unless FILE hashes to EXPECTED.
verify_sha256() {
	actual="$(sha256_of "$1")"
	if [ "$actual" != "$2" ]; then
		rm -f "$1"
		die "checksum mismatch for $(basename "$1"): got $actual, want $2"
	fi
}

install_macos() {
	if ! have brew; then
		log "Homebrew not found; installing it (this will prompt for your password)"
		brew_tmp="$(mktemp -d)"
		trap 'rm -rf "$brew_tmp"' EXIT
		curl -fsSL --proto '=https' --tlsv1.2 -o "$brew_tmp/install.sh" \
			"https://raw.githubusercontent.com/Homebrew/install/$HOMEBREW_INSTALL_COMMIT/install.sh" ||
			die "could not download the Homebrew installer"
		verify_sha256 "$brew_tmp/install.sh" "$HOMEBREW_INSTALL_SHA256"
		NONINTERACTIVE=1 /bin/bash "$brew_tmp/install.sh"
		# Homebrew's installer prints its own PATH instructions; try the two common prefixes so
		# the rest of this script (and the user's very next command) can find `brew` right away.
		if [ -x /opt/homebrew/bin/brew ]; then
			eval "$(/opt/homebrew/bin/brew shellenv)"
		elif [ -x /usr/local/bin/brew ]; then
			eval "$(/usr/local/bin/brew shellenv)"
		fi
	fi
	log "installing grove via Homebrew (this repo doubles as its own tap)"
	if ! brew tap | grep -qx gm2211/grove; then
		brew tap gm2211/grove https://github.com/gm2211/grove
	fi
	brew install gm2211/grove/grove
}

install_linux() {
	arch="$1"
	dest_dir="$HOME/.local/bin"
	if [ -w /usr/local/bin ]; then
		dest_dir="/usr/local/bin"
	fi
	mkdir -p "$dest_dir"

	repo="https://github.com/gm2211/grove"
	# GROVE_VERSION pins a release (e.g. v0.1.12); otherwise resolve "latest" to a tag ONCE, so the
	# tarball and the checksums file are guaranteed to come from the same release.
	version="${GROVE_VERSION:-}"
	if [ -z "$version" ]; then
		latest="$(curl -fsSL --proto '=https' --tlsv1.2 -o /dev/null -w '%{url_effective}' "$repo/releases/latest")" ||
			die "could not resolve the latest grove release (has a release been published yet?)"
		version="${latest##*/}"
	fi
	case "$version" in
	v[0-9]*) ;;
	*) die "unexpected grove release tag: '$version'" ;;
	esac
	log "fetching grove $version for linux/$arch"
	asset="grove_linux_${arch}.tar.gz"
	base="$repo/releases/download/$version"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/$asset" "$base/$asset" ||
		die "could not download $base/$asset"
	curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/checksums.txt" "$base/checksums.txt" ||
		die "could not download $base/checksums.txt; refusing to install an unverified binary"
	expected="$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/checksums.txt")"
	[ -n "$expected" ] || die "$asset is not listed in $version's checksums.txt"
	verify_sha256 "$tmp/$asset" "$expected"
	mv "$tmp/$asset" "$tmp/grove.tar.gz"
	tar -xzf "$tmp/grove.tar.gz" -C "$tmp" grove
	mv "$tmp/grove" "$dest_dir/grove"
	chmod +x "$dest_dir/grove"
	log "installed grove to $dest_dir/grove"
	case ":$PATH:" in
	*":$dest_dir:"*) ;;
	*) log "note: $dest_dir isn't on your PATH yet; add it to your shell profile" ;;
	esac
}

main() {
	os="$(detect_os)"
	arch="$(detect_arch)"

	case "$os" in
	darwin) install_macos ;;
	linux) install_linux "$arch" ;;
	esac

	log "grove installed. Next:"
	printf '\n'
	printf '    grove setup                         # join this Mac as a worker\n'
	printf '    grove install --role control-plane\n'
	printf '    grove install --role client --server https://<your-control-plane>.<tailnet>.ts.net:6130 --token <token>\n'
	printf '\n'
	log "grove setup discovers your control plane over Tailscale and guides approval."
}

main "$@"
