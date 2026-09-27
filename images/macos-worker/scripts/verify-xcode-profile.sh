#!/bin/bash
set -eu

developer_dir=$(xcode-select -p 2>/dev/null || true)
has_xcode=false
if [ -n "$developer_dir" ] && [ -d "$developer_dir/Platforms/MacOSX.platform" ]; then
  has_xcode=true
fi

case "${GROVE_XCODE_PROFILE:-false}:$has_xcode" in
  true:true|false:false)
    ;;
  true:false)
    echo 'FATAL: Xcode profile selected but the selected developer directory has no macOS SDK platform. Use a full Xcode image or select Xcode before building.'
    exit 1
    ;;
  false:true)
    echo 'FATAL: full Xcode is present but the explicit Xcode profile was not selected; build with -var-file=xcode.pkrvars.hcl.'
    exit 1
    ;;
  *)
    echo 'FATAL: GROVE_XCODE_PROFILE must be true or false.'
    exit 1
    ;;
esac
