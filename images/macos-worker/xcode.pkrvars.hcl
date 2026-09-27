# Explicit opt-in profile for jobs that need the full Xcode toolchain.
# Build with: packer build -var-file=xcode.pkrvars.hcl .
base_image    = "ghcr.io/cirruslabs/macos-sequoia-xcode:latest"
vm_name       = "grove-macos-xcode-worker"
disk_size_gb  = 150
xcode_profile = true
