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

install_macos() {
	if ! have brew; then
		log "Homebrew not found; installing it (this will prompt for your password)"
		NONINTERACTIVE=1 /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
		# Homebrew's installer prints its own PATH instructions; try the two common prefixes so
		# the rest of this script (and the user's very next command) can find `brew` right away.
		if [ -x /opt/homebrew/bin/brew ]; then
			eval "$(/opt/homebrew/bin/brew shellenv)"
		elif [ -x /usr/local/bin/brew ]; then
			eval "$(/usr/local/bin/brew shellenv)"
		fi
	fi
	log "installing grove via Homebrew (gm2211/tap/grove)"
	brew install gm2211/tap/grove
}

install_linux() {
	arch="$1"
	dest_dir="$HOME/.local/bin"
	if [ -w /usr/local/bin ]; then
		dest_dir="/usr/local/bin"
	fi
	mkdir -p "$dest_dir"

	log "fetching the latest grove release for linux/$arch"
	latest_url="https://github.com/gm2211/grove/releases/latest/download/grove_linux_${arch}.tar.gz"
	tmp="$(mktemp -d)"
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL -o "$tmp/grove.tar.gz" "$latest_url" ||
		die "could not download $latest_url (has a release been published yet?)"
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

	log "grove installed. Next, pick a role and run:"
	printf '\n'
	printf '    grove install --role worker --controller https://<your-control-plane>.<tailnet>.ts.net:6120\n'
	printf '    grove install --role control-plane\n'
	printf '    grove install --role client --server https://<your-control-plane>.<tailnet>.ts.net:6130 --token <token>\n'
	printf '\n'
	log "see docs/INSTALL.md for the full walkthrough, or hand this repo's docs/INSTALL.md to Claude Code."
}

main "$@"
