// grove Linux worker image: Tart VM (arm64 guest, Ubuntu) running a Nomad client (linux node_class)
// with Docker, plus Rosetta 2 binfmt registration so amd64 containers still run. See
// docs/IMAGES.md for the two-file Nomad config contract shared with images/macos-worker.

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
  default     = "ghcr.io/cirruslabs/ubuntu:latest"
  description = "arm64 Tart base image to clone from."
}

variable "vm_name" {
  type        = string
  default     = "grove-linux-worker"
  description = "Local Tart VM name produced by this build. Pushed to ghcr.io/gm2211/grove-linux-worker:<tag> as a separate `tart push` step — see docs/IMAGES.md."
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
  default = 60
}

variable "ssh_username" {
  type    = string
  default = "admin"
}

variable "ssh_password" {
  type    = string
  default = "admin"
}

source "tart-cli" "linux" {
  vm_base_name = var.base_image
  vm_name      = var.vm_name
  cpu_count    = var.cpu_count
  memory_gb    = var.memory_gb
  disk_size_gb = var.disk_size_gb
  headless     = true
  ssh_username = var.ssh_username
  ssh_password = var.ssh_password
  ssh_timeout  = "120s"
  # `rosetta = "rosetta"` only takes effect when whoever runs the VM also passes
  # `tart run --rosetta rosetta <vm>` (the fleet startup script does this — see
  # docs/IMAGES.md); it tells Tart to share the host's Rosetta translator at the guest tag
  # "rosetta" during provisioning too, so we can install the binfmt_misc registration here.
  rosetta = "rosetta"
}

build {
  sources = ["source.tart-cli.linux"]

  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo apt-get update -y",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y ca-certificates curl gnupg lsb-release git jq build-essential",
      "sudo install -m 0755 -d /etc/apt/keyrings",
    ]
  }

  # --- Docker Engine (official apt repo) --------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg",
      "echo \"deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(. /etc/os-release && echo \\\"$VERSION_CODENAME\\\") stable\" | sudo tee /etc/apt/sources.list.d/docker.list > /dev/null",
      "sudo apt-get update -y",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
      "sudo usermod -aG docker admin",
      "sudo systemctl enable docker",
    ]
  }

  # --- Nomad (HashiCorp apt repo) ----------------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "curl -fsSL https://apt.releases.hashicorp.com/gpg | sudo gpg --dearmor -o /etc/apt/keyrings/hashicorp.gpg",
      "echo \"deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(lsb_release -cs) main\" | sudo tee /etc/apt/sources.list.d/hashicorp.list > /dev/null",
      "sudo apt-get update -y",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y nomad gh",
    ]
  }

  # --- Tailscale (standalone daemon; joining the tailnet happens at boot via the fleet startup
  # script with an ephemeral auth key, not here — see images/macos-worker for the parallel rule) --
  provisioner "shell" {
    inline = [
      "set -eu",
      "curl -fsSL https://tailscale.com/install.sh | sudo sh",
      "sudo systemctl enable tailscaled",
    ]
  }

  # --- Nomad client config: static half only ------------------------------------------------------
  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo mkdir -p /etc/nomad.d /opt/nomad/data",
    ]
  }
  provisioner "file" {
    source      = "files/client.hcl"
    destination = "/tmp/client.hcl"
  }
  provisioner "shell" {
    inline = ["sudo mv /tmp/client.hcl /etc/nomad.d/client.hcl"]
  }
  provisioner "file" {
    source      = "files/grove-meta.hcl.placeholder"
    destination = "/tmp/grove-meta.hcl"
  }
  provisioner "shell" {
    inline = ["sudo mv /tmp/grove-meta.hcl /etc/nomad.d/grove-meta.hcl"]
  }
  provisioner "file" {
    source      = "files/nomad.service"
    destination = "/tmp/nomad.service"
  }
  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo mv /tmp/nomad.service /etc/systemd/system/nomad.service",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable nomad",
      # Do not `systemctl start` here: no servers configured yet during image build.
    ]
  }

  # --- Rosetta 2: register binfmt_misc so amd64 containers still run on this arm64 host ----------
  # Apple's Virtualization.framework shares the host Rosetta translator into the guest as a
  # virtiofs mount tagged "rosetta" (enabled by the `rosetta = "rosetta"` source field above, and
  # by `tart run --rosetta rosetta` at runtime). This script mounts it at /mnt/rosetta and
  # registers the well-known Rosetta binfmt_misc entry so the kernel routes x86_64 ELF exec()
  # through it — the same mechanism Apple documents for Linux VMs and that Lima/colima use.
  provisioner "file" {
    source      = "files/rosetta-binfmt.sh"
    destination = "/tmp/rosetta-binfmt.sh"
  }
  provisioner "file" {
    source      = "files/rosetta-binfmt.service"
    destination = "/tmp/rosetta-binfmt.service"
  }
  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo mkdir -p /mnt/rosetta",
      "sudo mv /tmp/rosetta-binfmt.sh /usr/local/sbin/rosetta-binfmt.sh",
      "sudo chmod 755 /usr/local/sbin/rosetta-binfmt.sh",
      "sudo mv /tmp/rosetta-binfmt.service /etc/systemd/system/rosetta-binfmt.service",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable rosetta-binfmt.service",
    ]
  }

  provisioner "shell" {
    inline = [
      "set -eu",
      "sudo apt-get clean",
      "sudo rm -rf /var/lib/apt/lists/*",
    ]
  }
}
