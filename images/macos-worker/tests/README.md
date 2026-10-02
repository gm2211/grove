# Real worker dispatch acceptance

`dispatch-matrix.py` runs the built Grove CLI/API against a disposable, guest-local Nomad
server/client. Run it inside a test VM, after the production startup script has configured the
selected developer directory and the node name has been set to `grove-matrix-lean`,
`grove-matrix-bundled`, or `grove-matrix-shared`. The script refuses a different node, multiple
nodes, or any existing jobs before changing server state.

Place a built `grove` executable alongside these files (or set `GROVE_MATRIX_BINARY` to its guest
path), then run `python3 dispatch-matrix.py lean`, `bundled`, or `shared`. No live Grove
credentials are needed: each run creates a private ephemeral credential and isolated state,
then removes them along with its Nomad jobs. Copy all three test files together.

Each row dispatches a C compile/run and a real Foundation/AppKit Xcode project build/run. The
lean row expects the Xcode job to fail with exit 78 and the full-Xcode-required diagnostic. All
rows verify returned allocation IDs, placement metadata, exit codes, and stdout/stderr. The
scripts use the developer directory selected by worker startup, not an injected SDK override.

This does not exercise live Orchard placement, simulator boot, or signing. Packer separately
runs `scripts/verify-nomad.sh` as a mandatory image-build gate, so a broken Nomad client cannot
produce a successful image build.
