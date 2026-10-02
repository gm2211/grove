// grove macOS worker image: Tart VM running a Nomad client (macos node_class) plus the toolchain
// build/agent jobs expect. Tailscale is installed but NOT joined here — the fleet startup script
// (internal/fleet) joins the tailnet with an ephemeral auth key at VM creation time. See
// docs/IMAGES.md for the full contract between this image and the fleet reconciler.

packer {
  required_plugins {
    tart = {
      version = "= 1.21.0"
      source  = "github.com/cirruslabs/tart"
    }
  }
}

variable "base_image" {
  type        = string
  default     = "ghcr.io/cirruslabs/macos-tahoe-vanilla@sha256:eeec54bfe1f076e27786c5d92b89187a05b1d109b5071eb2dcdf02d596e34640"
  description = "OCI reference of the Tart base image to clone from. The default and host-Xcode profiles share a minimal Tahoe image. Use xcode.pkrvars.hcl for bundled Xcode."
}

variable "vm_name" {
  type        = string
  default     = "grove-macos-worker"
  description = "Local Tart VM name produced by this build. Each explicit profile uses a separate name."
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
  type = number
  # The upstream vanilla template uses 50 GB. Keep 10 GB of
  # headroom for Grove's Nomad/Tailscale and general worker tools.
  default = 60
}

variable "xcode_profile" {
  type        = bool
  default     = false
  description = "Enable Xcode license and first-launch setup. Set only through xcode.pkrvars.hcl."
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
  vm_base_name     = var.base_image
  vm_name          = var.vm_name
  cpu_count        = var.cpu_count
  memory_gb        = var.memory_gb
  disk_size_gb     = var.disk_size_gb
  headless         = true
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "120s"
  pull_concurrency = 8
}

build {
  sources = ["source.tart-cli.macos"]

  # --- verify inherited image properties -----------------------------------------------------
  # The upstream vanilla and Xcode variants ship auto-login for `admin` and passwordless
  # sudo for the `admin` group. We only assert both
  # hold rather than reconfigure them, so a base-image change that drops either fails the build
  # loudly instead of silently shipping an image that needs a password at the console.
  provisioner "shell" {
    inline = [
      "set -eu",
      "echo '==> verifying passwordless sudo for admin (inherited from the selected CirrusLabs base)'",
      "sudo -n true || (echo 'FATAL: admin cannot sudo without a password; base image changed?' && exit 1)",
      "echo '==> verifying auto-login is configured (inherited from the selected CirrusLabs base)'",
      "defaults read /Library/Preferences/com.apple.loginwindow autoLoginUser || (echo 'FATAL: autoLoginUser not set; base image changed?' && exit 1)",
    ]
  }

  # Vanilla has no CI toolchain. Install CLT/Homebrew and only the guest transport first.
  # The script also works with a pre-provisioned full-Xcode base.
  provisioner "shell" {
    script = "scripts/bootstrap-vanilla.sh"
  }

  provisioner "file" {
    source      = "files/tart-guest-daemon.plist"
    destination = "/tmp/tart-guest-daemon.plist"
  }
  provisioner "file" {
    source      = "files/tart-guest-agent.plist"
    destination = "/tmp/tart-guest-agent.plist"
  }
  provisioner "shell" {
    inline = [
      "sudo install -o root -g wheel -m 0644 /tmp/tart-guest-daemon.plist /Library/LaunchDaemons/org.cirruslabs.tart-guest-daemon.plist",
      "sudo install -o root -g wheel -m 0644 /tmp/tart-guest-agent.plist /Library/LaunchAgents/org.cirruslabs.tart-guest-agent.plist",
      "rm /tmp/tart-guest-daemon.plist /tmp/tart-guest-agent.plist",
    ]
  }

  provisioner "shell" {
    environment_vars = ["GROVE_XCODE_PROFILE=${var.xcode_profile}"]
    script           = "scripts/verify-xcode-profile.sh"
  }

  # --- Homebrew toolchain ---------------------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      # Packer's SSH session is a non-login shell: Homebrew's /opt/homebrew/bin is NOT on PATH
      # there (only in login shells via /etc/paths.d). Put it first explicitly. Bootstrap above
      # owns CLT and Homebrew installation; do not introduce a second moving installer URL.
      "export PATH=/opt/homebrew/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin",
      "xcrun --find clang >/dev/null 2>&1 || { echo 'FATAL: Apple Command Line Tools are missing. Homebrew needs them if any formula must build from source; install with xcode-select --install, then retry.'; exit 1; }",
      "command -v brew >/dev/null 2>&1 || { echo 'FATAL: vanilla bootstrap did not install Homebrew'; exit 1; }",
      "export HOMEBREW_NO_AUTO_UPDATE=1",
      "export HOMEBREW_NO_INSTALL_CLEANUP=1",
      "echo '==> installing git, gh, jq, node, python'",
      "brew install git gh jq node python@3.12",
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

  # Native ARM64 Nomad with the pinned VM topology fix. Built once on the image builder;
  # compiler/source/module caches never enter the worker image.
  provisioner "file" {
    source      = "artifacts/nomad"
    destination = "/tmp/grove-nomad"
  }
  provisioner "file" {
    source      = "artifacts/metadata.json"
    destination = "/tmp/grove-nomad-metadata.json"
  }
  provisioner "file" {
    source      = "artifacts/NOMAD-LICENSE.txt"
    destination = "/tmp/grove-nomad-LICENSE.txt"
  }
  provisioner "shell" {
    inline = [
      "set -eu",
      "export PATH=/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin",
      "test \"$(shasum -a 256 /tmp/grove-nomad | awk '{print $1}')\" = \"$(jq -r .binary_sha256 /tmp/grove-nomad-metadata.json)\"",
      "sudo mkdir -p /usr/local/bin /usr/local/share/grove",
      "sudo install -o root -g wheel -m 0755 /tmp/grove-nomad /usr/local/bin/nomad",
      "sudo install -o root -g wheel -m 0644 /tmp/grove-nomad-metadata.json /usr/local/share/grove/nomad-build.json",
      "sudo install -o root -g wheel -m 0644 /tmp/grove-nomad-LICENSE.txt /usr/local/share/grove/NOMAD-LICENSE.txt",
      "rm /tmp/grove-nomad /tmp/grove-nomad-metadata.json /tmp/grove-nomad-LICENSE.txt",
      "/usr/local/bin/nomad version",
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
  dynamic "provisioner" {
    for_each = var.xcode_profile ? [true] : []
    labels   = ["shell"]
    content {
      inline = [
        "set -eu",
        "command -v xcodebuild >/dev/null 2>&1 || { echo 'FATAL: Xcode profile selected but xcodebuild is unavailable in the configured base image.'; exit 1; }",
        "echo '==> accepting Xcode license and running first-launch package installs'",
        "sudo xcodebuild -license accept",
        "sudo xcodebuild -runFirstLaunch",
      ]
    }
  }

  # Never publish an image whose Nomad agent only appears loaded but cannot schedule work.
  provisioner "shell" {
    script = "scripts/verify-nomad.sh"
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
