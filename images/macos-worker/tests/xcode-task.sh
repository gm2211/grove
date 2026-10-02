#!/bin/bash
set -euo pipefail

# Use the developer directory selected by the real worker startup, without override.
mode="${GROVE_MATRIX_MODE:?matrix mode required}"
unset DEVELOPER_DIR
developer_dir="$(xcode-select -p)"
echo "MATRIX_MODE=$mode"
echo "SELECTED_DEVELOPER_DIR=$developer_dir"
case "$mode:$developer_dir" in
  lean:/Library/Developer/CommandLineTools) ;;
  shared:'/Volumes/My Shared Files/grove-xcode.app/Contents/Developer') ;;
  bundled:/Applications/*.app/Contents/Developer) ;;
  *) echo "Unexpected selected toolchain for $mode: $developer_dir" >&2; exit 2 ;;
esac
if ! xcodebuild -version; then
  if [[ "$mode" == lean ]]; then
    echo "GROVE_XCODE_REQUIRED: full Xcode is required for xcodebuild project builds" >&2
    exit 78
  fi
  exit 2
fi
if [[ "$mode" == lean ]]; then
  echo "Unexpected full Xcode on lean worker" >&2
  exit 2
fi

tmp="$(mktemp -d "${TMPDIR:-/tmp}/grove-xcode-matrix.XXXXXX")"
cleanup() { rm -rf "$tmp"; }
trap cleanup EXIT HUP INT TERM
project="$tmp/XcodeFixture.xcodeproj"
mkdir -p "$project" "$tmp/Sources"

cat > "$tmp/Sources/main.swift" <<'SWIFT'
import Foundation
import AppKit

print("GROVE_XCODE_OK os=\(ProcessInfo.processInfo.operatingSystemVersionString) color=\(NSColor.blue.description)")
SWIFT

cat > "$project/project.pbxproj" <<'PBX'
// !$*UTF8*$!
{
	archiveVersion = 1;
	classes = {};
	objectVersion = 56;
	objects = {
		A10000000000000000000001 /* main.swift in Sources */ = {isa = PBXBuildFile; fileRef = A10000000000000000000002 /* main.swift */; };
		A10000000000000000000002 /* main.swift */ = {isa = PBXFileReference; lastKnownFileType = sourcecode.swift; path = main.swift; sourceTree = "<group>"; };
		A10000000000000000000003 /* XcodeFixture */ = {isa = PBXFileReference; explicitFileType = compiled.mach-o.executable; includeInIndex = 0; path = XcodeFixture; sourceTree = BUILT_PRODUCTS_DIR; };
		A10000000000000000000004 = {isa = PBXGroup; children = (A10000000000000000000005 /* Sources */, A10000000000000000000006 /* Products */,); sourceTree = "<group>"; };
		A10000000000000000000005 /* Sources */ = {isa = PBXGroup; children = (A10000000000000000000002 /* main.swift */,); path = Sources; sourceTree = "<group>"; };
		A10000000000000000000006 /* Products */ = {isa = PBXGroup; children = (A10000000000000000000003 /* XcodeFixture */,); name = Products; sourceTree = "<group>"; };
		A10000000000000000000007 /* Sources */ = {isa = PBXSourcesBuildPhase; buildActionMask = 2147483647; files = (A10000000000000000000001 /* main.swift in Sources */,); runOnlyForDeploymentPostprocessing = 0; };
		A10000000000000000000008 /* XcodeFixture */ = {isa = PBXNativeTarget; buildConfigurationList = A1000000000000000000000B; buildPhases = (A10000000000000000000007 /* Sources */,); buildRules = (); dependencies = (); name = XcodeFixture; productName = XcodeFixture; productReference = A10000000000000000000003 /* XcodeFixture */; productType = "com.apple.product-type.tool"; };
		A10000000000000000000009 /* Project */ = {isa = PBXProject; attributes = {LastUpgradeCheck = 1500; }; buildConfigurationList = A1000000000000000000000C; compatibilityVersion = "Xcode 14.0"; developmentRegion = en; hasScannedForEncodings = 0; mainGroup = A10000000000000000000004; productRefGroup = A10000000000000000000006 /* Products */; projectDirPath = ""; projectRoot = ""; targets = (A10000000000000000000008 /* XcodeFixture */,); };
		A1000000000000000000000A /* Release */ = {isa = XCBuildConfiguration; buildSettings = {MACOSX_DEPLOYMENT_TARGET = 13.0; PRODUCT_NAME = "$(TARGET_NAME)"; SDKROOT = macosx; SWIFT_VERSION = 5.0; }; name = Release; };
		A1000000000000000000000D /* Debug */ = {isa = XCBuildConfiguration; buildSettings = {MACOSX_DEPLOYMENT_TARGET = 13.0; PRODUCT_NAME = "$(TARGET_NAME)"; SDKROOT = macosx; SWIFT_VERSION = 5.0; }; name = Debug; };
		A1000000000000000000000E /* Release */ = {isa = XCBuildConfiguration; buildSettings = {MACOSX_DEPLOYMENT_TARGET = 13.0; PRODUCT_NAME = "$(TARGET_NAME)"; SDKROOT = macosx; SWIFT_VERSION = 5.0; }; name = Release; };
		A1000000000000000000000B = {isa = XCConfigurationList; buildConfigurations = (A1000000000000000000000D /* Debug */, A1000000000000000000000A /* Release */,); defaultConfigurationIsVisible = 0; defaultConfigurationName = Release; };
		A1000000000000000000000C = {isa = XCConfigurationList; buildConfigurations = (A1000000000000000000000D /* Debug */, A1000000000000000000000E /* Release */,); defaultConfigurationIsVisible = 0; defaultConfigurationName = Release; };
	};
	rootObject = A10000000000000000000009 /* Project */;
}
PBX

build_log="$tmp/xcodebuild.log"
echo "BUILD_START mode=$mode project=Foundation+AppKit command-line target"
if xcodebuild -project "$project" -scheme XcodeFixture -configuration Debug -destination generic/platform=macOS \
  -derivedDataPath "$tmp/DerivedData" CODE_SIGNING_ALLOWED=NO build >"$build_log" 2>&1; then
  result=0
else
  result=$?
  echo "BUILD_FAILED mode=$mode (last 80 xcodebuild log lines follow)" >&2
  tail -n 80 "$build_log" >&2
  exit "$result"
fi
grep -E '(^| )warning:|(^| )error:|BUILD SUCCEEDED|BUILD FAILED' "$build_log" | tail -n 30 || true
binary="$tmp/DerivedData/Build/Products/Debug/XcodeFixture"
if [[ ! -x "$binary" ]]; then
  echo "ERROR: xcodebuild reported success but expected executable is missing: $binary" >&2
  tail -n 80 "$build_log" >&2
  exit 1
fi
echo "EXECUTE_START mode=$mode"
"$binary"
echo "MATRIX_RESULT=PASS mode=$mode"
