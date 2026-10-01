#!/bin/bash
set -euo pipefail

echo "CLT_BASELINE_START matrix_mode=${GROVE_MATRIX_MODE:?dispatch env not injected}"
echo "DEVELOPER_DIR=${DEVELOPER_DIR:-$(xcode-select -p 2>/dev/null || echo unknown)}"
clang_path="$(xcrun --find clang)"
echo "CLANG=$clang_path"
tmp="$(mktemp -d "${TMPDIR:-/tmp}/grove-clt-baseline.XXXXXX")"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT HUP INT TERM
cat > "$tmp/probe.c" <<'C'
#include <stdio.h>
int main(void) {
    puts("GROVE_CLT_OK");
    return 0;
}
C
clang "$tmp/probe.c" -o "$tmp/probe"
"$tmp/probe"
echo "CLT_BASELINE_RESULT=PASS matrix_mode=${GROVE_MATRIX_MODE:?dispatch env not injected}"
