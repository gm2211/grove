#!/bin/bash
set -euo pipefail

readonly NOMAD_VERSION="2.0.7"
readonly NOMAD_COMMIT="9dcbdc5e64ecc4ec63e42b225af729c87c87cf83"
readonly NOMAD_SOURCE_SHA256="a250613ab718df0f848d4c982884d47f7c80cbb0860ee298475220eb587153a8"
readonly PATCH_SHA256="f6ce70b2bbc77a69e9672491211f2c9a21b89d51266fba333b6b60950d646191"
readonly GO_VERSION="go1.27.1"

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
artifact_dir="${repo_root}/images/macos-worker/artifacts"
patch_file="${repo_root}/images/macos-worker/patches/nomad-darwin-single-perflevel.patch"

fail() {
	printf 'build-worker-nomad: %s\n' "$*" >&2
	exit 1
}

[[ "$(uname -s)" == "Darwin" ]] || fail "requires a macOS build host"
[[ "$(uname -m)" == "arm64" ]] || fail "requires a native Apple Silicon build host"
[[ -f "${patch_file}" ]] || fail "missing tracked patch: ${patch_file}"

actual_go_version="$(go env GOVERSION)"
[[ "${actual_go_version}" == "${GO_VERSION}" ]] || fail "requires ${GO_VERSION}; found ${actual_go_version}"
[[ "$(go env GOOS)" == "darwin" && "$(go env GOARCH)" == "arm64" ]] || \
	fail "Go must target darwin/arm64 natively"

actual_patch_sha256="$(shasum -a 256 "${patch_file}" | awk '{print $1}')"
[[ "${actual_patch_sha256}" == "${PATCH_SHA256}" ]] || fail "tracked patch checksum mismatch"
build_script_sha256="$(shasum -a 256 "${script_dir}/$(basename "${BASH_SOURCE[0]}")" | awk '{print $1}')"

cached_binary="${artifact_dir}/nomad"
cached_metadata="${artifact_dir}/metadata.json"
cached_license="${artifact_dir}/NOMAD-LICENSE.txt"
metadata_value() {
	/usr/bin/plutil -extract "$1" raw -o - "${cached_metadata}" 2>/dev/null || true
}
if [[ -x "${cached_binary}" && -f "${cached_metadata}" && -f "${cached_license}" ]]; then
	cached_binary_sha256="$(shasum -a 256 "${cached_binary}" | awk '{print $1}')"
	cached_license_sha256="$(shasum -a 256 "${cached_license}" | awk '{print $1}')"
	if [[ "$(metadata_value nomad_version)" == "${NOMAD_VERSION}+grove.1" && \
		"$(metadata_value source_commit)" == "${NOMAD_COMMIT}" && \
		"$(metadata_value source_archive_sha256)" == "${NOMAD_SOURCE_SHA256}" && \
		"$(metadata_value patch_sha256)" == "${actual_patch_sha256}" && \
		"$(metadata_value build_script_sha256)" == "${build_script_sha256}" && \
		"$(metadata_value go_version)" == "${GO_VERSION}" && \
		"$(metadata_value target)" == "darwin/arm64" && \
		"$(metadata_value binary_sha256)" == "${cached_binary_sha256}" && \
		"$(metadata_value license_sha256)" == "${cached_license_sha256}" ]]; then
		version_output="$("${cached_binary}" version)"
		[[ "${version_output}" == *"Nomad v${NOMAD_VERSION}+grove.1"* ]] || \
			fail "cached artifact version metadata is invalid"
		[[ "${version_output}" == *"Revision ${NOMAD_COMMIT}"* ]] || \
			fail "cached artifact source revision is invalid"
		printf 'Using verified cached artifact %s\n' "${cached_binary}"
		printf 'SHA-256 %s\n' "${cached_binary_sha256}"
		exit 0
	fi
fi

build_root="$(mktemp -d "${TMPDIR:-/tmp}/grove-nomad-build.XXXXXX")"
cleanup() {
	status=$?
	trap - EXIT HUP INT TERM
	chmod -R u+rwX "${build_root}" 2>/dev/null || true
	rm -rf "${build_root}"
	exit "${status}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

archive="${build_root}/nomad-source.tar.gz"
source_dir="${build_root}/source"
mkdir -p "${source_dir}" "${build_root}/gomodcache" "${build_root}/gocache"

curl --fail --location --retry 2 --silent --show-error \
	"https://codeload.github.com/hashicorp/nomad/tar.gz/${NOMAD_COMMIT}" \
	--output "${archive}"
actual_source_sha256="$(shasum -a 256 "${archive}" | awk '{print $1}')"
[[ "${actual_source_sha256}" == "${NOMAD_SOURCE_SHA256}" ]] || \
	fail "Nomad source archive checksum mismatch: ${actual_source_sha256}"

tar -xzf "${archive}" --strip-components=1 -C "${source_dir}"
git -C "${source_dir}" apply "${patch_file}"

export GOMODCACHE="${build_root}/gomodcache"
export GOCACHE="${build_root}/gocache"
export GOTOOLCHAIN=local
export CGO_ENABLED=1
export GOOS=darwin
export GOARCH=arm64
export GOFLAGS=-mod=readonly

cd "${source_dir}"
go test ./client/lib/numalib \
	-run '^(TestNormalizeAppleSiliconCoreCounts|TestScanTopology)$' -count=1

build_date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
xcode_version="$(xcodebuild -version | tr '\n' ' ')"
sdk_version="$(xcrun --show-sdk-version)"
temporary_binary="${build_root}/nomad"
go build -trimpath -buildvcs=false \
	-ldflags "-s -w -X github.com/hashicorp/nomad/version.GitCommit=${NOMAD_COMMIT} -X github.com/hashicorp/nomad/version.GitDescribe= -X github.com/hashicorp/nomad/version.VersionMetadata=grove.1 -X github.com/hashicorp/nomad/version.BuildDate=${build_date}" \
	-o "${temporary_binary}" .

version_output="$("${temporary_binary}" version)"
[[ "${version_output}" == *"Nomad v${NOMAD_VERSION}+grove.1"* ]] || \
	fail "built artifact has unexpected version metadata"
[[ "${version_output}" == *"Revision ${NOMAD_COMMIT}"* ]] || \
	fail "built artifact has unexpected source revision"

mkdir -p "${artifact_dir}"
temporary_metadata="${build_root}/metadata.json"
temporary_license="${build_root}/NOMAD-LICENSE.txt"
binary_sha256="$(shasum -a 256 "${temporary_binary}" | awk '{print $1}')"
license_sha256="$(shasum -a 256 "${source_dir}/LICENSE" | awk '{print $1}')"
cp "${source_dir}/LICENSE" "${temporary_license}"
cat > "${temporary_metadata}" <<EOF
{
  "nomad_version": "${NOMAD_VERSION}+grove.1",
  "source_commit": "${NOMAD_COMMIT}",
  "source_archive_url": "https://codeload.github.com/hashicorp/nomad/tar.gz/${NOMAD_COMMIT}",
  "source_archive_sha256": "${actual_source_sha256}",
  "patch_file": "images/macos-worker/patches/nomad-darwin-single-perflevel.patch",
  "patch_sha256": "${actual_patch_sha256}",
  "build_script_sha256": "${build_script_sha256}",
  "go_version": "${GO_VERSION}",
  "target": "darwin/arm64",
  "cgo_enabled": true,
  "module_mode": "readonly (go.mod/go.sum)",
  "xcode_version": "${xcode_version}",
  "macos_version": "$(sw_vers -productVersion)",
  "macos_sdk_version": "${sdk_version}",
  "tested_packages": ["client/lib/numalib"],
  "built_at_utc": "${build_date}",
  "license_sha256": "${license_sha256}",
  "binary_sha256": "${binary_sha256}"
}
EOF

install -m 0755 "${temporary_binary}" "${build_root}/nomad.artifact"
mv -f "${build_root}/nomad.artifact" "${artifact_dir}/nomad"
mv -f "${temporary_license}" "${artifact_dir}/NOMAD-LICENSE.txt"
mv -f "${temporary_metadata}" "${artifact_dir}/metadata.json"
printf 'Built %s\n' "${artifact_dir}/nomad"
printf '%s\n' "${version_output}"
printf 'SHA-256 %s\n' "${binary_sha256}"
