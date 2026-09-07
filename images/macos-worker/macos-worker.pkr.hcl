// grove macOS worker image: Tart VM running a Nomad client (macos node_class) plus the toolchain
// build/agent jobs expect. Tailscale is installed but NOT joined here — the fleet startup script
// (internal/fleet) joins the tailnet with an ephemeral auth key at VM creation time. See
// docs/IMAGES.md for the full contract between this image and the fleet reconciler.

packer {
  required_plugins {
    tart = {
      version = ">= 1.13.0"
      source  = "github.com/cirruslabs/tart"
    }
  }
}

variable "base_image" {
  type        = string
  default     = "ghcr.io/cirruslabs/macos-sequoia-xcode:latest"
  description = "OCI reference of the Tart base image to clone from. Must already have Xcode installed (see cirruslabs/macos-image-templates)."
}

variable "vm_name" {
  type        = string
  default     = "grove-macos-worker"
  description = "Local Tart VM name produced by this build. Pushed to ghcr.io/gm2211/grove-macos-worker:<tag> as a separate `tart push` step — see docs/IMAGES.md."
}

variable "cpu_count" {
  type    = number
  default = 4
}

variable "memory_gb" {
  type    = number
  default = 8
}

variable "disk_size_gb" {
  type    = number
  default = 150
}

variable "ssh_username" {
  type        = string
  default     = "admin"
  description = "Tart base images use admin/admin; grove keeps that account rather than provisioning a new one."
}

variable "ssh_password" {
  type    = string
  default = "admin"
}

source "tart-cli" "macos" {
  vm_base_name = var.base_image
  vm_name      = var.vm_name
  cpu_count    = var.cpu_count
  memory_gb    = var.memory_gb
  disk_size_gb = var.disk_size_gb
  headless     = true
  ssh_username = var.ssh_username
  ssh_password = var.ssh_password
  ssh_timeout  = "120s"
}

build {
  sources = ["source.tart-cli.macos"]

  # --- verify inherited image properties -----------------------------------------------------
  # cirruslabs/macos-image-templates' xcode variant is built FROM macos-sequoia-base, which already
  # ships auto-login for `admin` and passwordless sudo for the `admin` group. We only assert both
  # hold rather than reconfigure them, so a base-image change that drops either fails the build
  # loudly instead of silently shipping an image that needs a password at the console.
  provisioner "shell" {
    inline = [
      "set -eu",
      "echo '==> verifying passwordless sudo for admin (inherited from macos-sequoia-base)'",
      "sudo -n true || (echo 'FATAL: admin cannot sudo without a password; base image changed?' && exit 1)",
      "echo '==> verifying auto-login is configured (inherited from macos-sequoia-base)'",
      "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser || (echo 'FATAL: autoLoginUser not set; base image changed?' && exit 1)",
    ]
  }

  # --- Homebrew toolchain ---------------------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      # Packer's SSH session is a non-login shell: Homebrew's /opt/homebrew/bin is NOT on PATH
      # there (only in login shells via /etc/paths.d). Put it first explicitly, and install
      # Homebrew if the base image somehow lacks it.
      "export PATH=/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
      "if ! command -v brew >/dev/null 2>&1; then echo '==> Homebrew missing, installing'; NONINTERACTIVE=1 /bin/bash -c \"$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"; fi",
      "export HOMEBREW_NO_AUTO_UPDATE=1",
      "export HOMEBREW_NO_INSTALL_CLEANUP=1",
      "echo '==> installing nomad, git, gh, jq, node, python'",
      "brew tap hashicorp/tap",
      "brew install hashicorp/tap/nomad git gh jq node python@3.12",
      # Tailscale: install the standalone (non-App-Store) build via the `tailscale` brew FORMULA,
      # not the `tailscale-app` CASK. The cask is the sandboxed Mac-App-Store-equivalent GUI build
      # that only starts once a user is logged into the GUI session; the formula ships a plain
      # tailscaled LaunchDaemon that behaves like the Linux daemon and starts headless before
      # login, which is what a worker VM needs (see ARCHITECTURE.md: 'Tailscale standalone variant
      # (not App Store — that one doesn't start before login)'). Verified against Tailscale's own
      # 'Three ways to run Tailscale on macOS' docs and formulae.brew.sh/formula/tailscale.
      "echo '==> installing tailscale (brew formula, not cask)'",
      "brew install tailscale",
      "sudo /opt/homebrew/bin/brew services start tailscale || true", # tailscaled itself; `tailscale up` (join) is done by the fleet startup script with an ephemeral auth key.
    ]
  }

  # --- Nomad client config: static half only ----------------------------------------------------
  # client.hcl carries everything that does NOT change per-boot. The dynamic half (`servers`,
  # the pool/host meta) is written by the fleet startup script into grove-meta.hcl right before
  # `nomad agent` starts, and gets replaced on every VM recreation. See docs/IMAGES.md.
  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo mkdir -p /usr/local/etc/nomad.d",
      "sudo mkdir -p /opt/nomad/data",
    ]
  }

  provisioner "file" {
    source      = "files/client.hcl"
    destination = "/tmp/client.hcl"
  }
  provisioner "shell" {
    inline = ["sudo mv /tmp/client.hcl /usr/local/etc/nomad.d/client.hcl"]
  }

  # Empty placeholder so `nomad agent -config /usr/local/etc/nomad.d` has something to merge with
  # before the fleet startup script overwrites it with real servers/meta on first boot.
  provisioner "file" {
    source      = "files/grove-meta.hcl.placeholder"
    destination = "/tmp/grove-meta.hcl"
  }
  provisioner "shell" {
    inline = ["sudo mv /tmp/grove-meta.hcl /usr/local/etc/nomad.d/grove-meta.hcl"]
  }

  provisioner "file" {
    source      = "files/com.grove.nomad.plist"
    destination = "/tmp/com.grove.nomad.plist"
  }
  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo mv /tmp/com.grove.nomad.plist /Library/LaunchDaemons/com.grove.nomad.plist",
      "sudo chown root:wheel /Library/LaunchDaemons/com.grove.nomad.plist",
      "sudo chmod 644 /Library/LaunchDaemons/com.grove.nomad.plist",
      # Do NOT bootstrap/kickstart here: nomad has no server to join to yet during the image
      # build, and would just crash-loop into KeepAlive. `launchctl load` happens on first real
      # boot (RunAtLoad=true in the plist takes care of it once servers are configured).
    ]
  }

  # --- power management: never sleep, never lock the console ----------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "echo '==> disabling sleep/screensaver so long-running jobs are never suspended'",
      "sudo systemsetup -setsleep Never || true",
      "sudo systemsetup -setcomputersleep Never || true",
      "sudo systemsetup -setdisplaysleep Never || true",
      "sudo pmset -a sleep 0 disksleep 0 displaysleep 0 womp 0 || true",
      "defaults -currentHost write com.apple.screensaver idleTime 0",
      "sudo defaults write /Library/Preferences/com.apple.screensaver askForPassword -int 0",
    ]
  }

  # --- pre-warm Xcode so the first job doesn't pay the license/first-launch tax ----------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "echo '==> accepting Xcode license and running first-launch package installs'",
      "sudo xcodebuild -license accept",
      "sudo xcodebuild -runFirstLaunch",
    ]
  }

  # --- cleanup ---------------------------------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "export PATH=/opt/homebrew/bin:/opt/homebrew/sbin:$PATH",
      "brew cleanup -s || true",
      "rm -rf $(brew --cache) || true",
      "sudo rm -rf /private/var/log/*.log || true",
    ]
  }
}
