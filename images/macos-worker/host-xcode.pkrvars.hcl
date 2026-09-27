# Xcode is shared read-only by Orchard at VM startup, not copied into this image.
# Choose a base OS that meets the selected host Xcode's LSMinimumSystemVersion.
base_image    = "ghcr.io/cirruslabs/macos-tahoe-base:latest"
vm_name       = "grove-macos-host-xcode-worker"
disk_size_gb  = 60
xcode_profile = false
