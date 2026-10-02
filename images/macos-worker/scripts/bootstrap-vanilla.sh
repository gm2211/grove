#!/bin/bash
set -euo pipefail

# Prepare a Cirrus Labs vanilla macOS guest for Grove's regular Packer provisioners.
# Keep existing full-Xcode selections intact; only install/select CLT if xcrun lacks clang.

readonly CLT_MARKER="/tmp/.com.apple.dt.CommandLineTools.installondemand.in-progress"
readonly HOMEBREW_INSTALL_COMMIT="f1f3f44a86c1cf81e45ec1caa43ab122a923c87a"

marker_owned=0
softwareupdate_log=""
homebrew_installer=""
paths_file=""

cleanup() {
  if [[ "$marker_owned" -eq 1 ]]; then
    rm -f "$CLT_MARKER"
  fi
  [[ -z "$softwareupdate_log" ]] || rm -f "$softwareupdate_log"
  [[ -z "$homebrew_installer" ]] || rm -f "$homebrew_installer"
  [[ -z "$paths_file" ]] || rm -f "$paths_file"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM HUP

export PATH="/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"
export HOMEBREW_NO_AUTO_UPDATE=1
export HOMEBREW_NO_INSTALL_CLEANUP=1

version_gt() {
  /usr/bin/awk -v left="$1" -v right="$2" 'BEGIN {
    na = split(left, a, "\\."); nb = split(right, b, "\\.");
    count = na > nb ? na : nb;
    for (i = 1; i <= count; i++) {
      x = (a[i] == "" ? 0 : a[i] + 0);
      y = (b[i] == "" ? 0 : b[i] + 0);
      if (x > y) exit 0;
      if (x < y) exit 1;
    }
    exit 1;
  }'
}

install_command_line_tools() {
  echo "==> Apple Command Line Tools are missing; finding the latest softwareupdate label"

  # The marker is the documented install-on-demand switch used by softwareupdate to
  # expose CLT as an update. Create it atomically and only remove it if this script owns it.
  if (set -C; : > "$CLT_MARKER") 2>/dev/null; then
    marker_owned=1
  fi

  softwareupdate_log="$(mktemp "${TMPDIR:-/tmp}/grove-softwareupdate-list.XXXXXX")"
  if ! softwareupdate --list >"$softwareupdate_log" 2>&1; then
    echo "FATAL: softwareupdate --list failed while looking for Command Line Tools." >&2
    cat "$softwareupdate_log" >&2
    return 1
  fi

  local latest_label="" latest_version="" label version
  while IFS= read -r label; do
    version="${label#Command Line Tools for Xcode}"
    version="${version#-}"
    version="${version# }"
    # Apple labels use dotted Xcode versions, sometimes followed by a build suffix.
    version="${version%%[-_]*}"
    [[ "$version" =~ ^[0-9]+(\.[0-9]+)*$ ]] || continue
    if [[ -z "$latest_label" ]] || version_gt "$version" "$latest_version"; then
      latest_label="$label"
      latest_version="$version"
    fi
  done < <(sed -nE 's/^[[:space:]]*\*[[:space:]]*Label:[[:space:]]*(Command Line Tools for Xcode[- ][^,[:space:]]+).*/\1/p' "$softwareupdate_log")

  if [[ -z "$latest_label" ]]; then
    echo "FATAL: softwareupdate did not list a matching 'Command Line Tools for Xcode-*' label." >&2
    cat "$softwareupdate_log" >&2
    return 1
  fi

  echo "==> installing $latest_label"
  sudo softwareupdate --install "$latest_label" --verbose
  sudo xcode-select --switch /Library/Developer/CommandLineTools
  rm -f "$softwareupdate_log"
  softwareupdate_log=""

  if ! /usr/bin/xcrun --find clang >/dev/null 2>&1; then
    echo "FATAL: Command Line Tools installation completed, but xcrun still cannot find clang." >&2
    return 1
  fi
}

if /usr/bin/xcrun --find clang >/dev/null 2>&1; then
  # Explicitly keep the currently selected developer directory (which can be full Xcode).
  selected_developer_dir="$(/usr/bin/xcode-select -p)"
  echo "==> preserving selected developer directory: $selected_developer_dir"
  sudo /usr/bin/xcode-select --switch "$selected_developer_dir"
else
  install_command_line_tools
fi

if [[ ! -x /opt/homebrew/bin/brew ]]; then
  echo "==> installing Homebrew from pinned official installer commit $HOMEBREW_INSTALL_COMMIT"
  homebrew_installer="$(mktemp "${TMPDIR:-/tmp}/grove-homebrew-install.XXXXXX")"
  curl --fail --silent --show-error --location \
    "https://raw.githubusercontent.com/Homebrew/install/${HOMEBREW_INSTALL_COMMIT}/install.sh" \
    --output "$homebrew_installer"
  NONINTERACTIVE=1 /bin/bash "$homebrew_installer"
fi

if [[ ! -x /opt/homebrew/bin/brew ]]; then
  echo "FATAL: Homebrew installer completed without /opt/homebrew/bin/brew." >&2
  exit 1
fi

# Packer provisioning uses non-login shells, so make Homebrew available system-wide too.
paths_file="$(mktemp "${TMPDIR:-/tmp}/grove-homebrew-paths.XXXXXX")"
printf '%s\n' /opt/homebrew/bin /opt/homebrew/sbin > "$paths_file"
if ! sudo cmp -s "$paths_file" /etc/paths.d/10-homebrew; then
  sudo install -o root -g wheel -m 0644 "$paths_file" /etc/paths.d/10-homebrew
fi
rm -f "$paths_file"
paths_file=""

echo "==> installing Tart guest agent formula"
if ! brew list --formula --versions tart-guest-agent >/dev/null 2>&1; then
  brew tap openai/tools
  brew install openai/tools/tart-guest-agent
fi

echo "VANILLA_BOOTSTRAP_OK"
