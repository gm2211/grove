# Xcode is shared read-only by Orchard at VM startup, not copied into this image.
# Choose a base OS that meets the selected host Xcode's LSMinimumSystemVersion.
# Inherit the same minimal vanilla-derived image as the default worker.
vm_name       = "grove-macos-host-xcode-worker"
disk_size_gb  = 60
xcode_profile = false
