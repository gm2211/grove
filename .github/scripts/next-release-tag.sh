#!/bin/bash
# Decide whether commit <sha> should be released, for autorelease.yml.
#
#   next-release-tag.sh <sha>
#
# Prints "tag=vX.Y.Z" (the latest v* tag with its patch number raised) when <sha> is the tip of
# origin/main and something a release ships changed since that tag; prints "tag=" otherwise, with
# the reason on stderr. Run in a full clone (tags and history).
set -euo pipefail

sha=${1:?usage: next-release-tag.sh <sha>}

# What a release is built from. Not docs, spec/, images/ or Formula/: the Formula commit that
# goreleaser pushes after a release must not cut another one.
paths=(cmd internal ui nomad scripts go.mod go.sum .goreleaser.yaml)

skip() { echo "autorelease: $1" >&2; echo "tag="; exit 0; }

tip=$(git rev-parse origin/main)
[ "$sha" = "$tip" ] || skip "$sha is not main's tip ($tip); the newer commit's run releases it"

latest=$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-version:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | head -1 || true)
[ -n "$latest" ] || skip "no vX.Y.Z tag to continue from; push the first one by hand"

git merge-base --is-ancestor "$latest" "$sha" || skip "$latest is not an ancestor of $sha"
if git diff --quiet "$latest" "$sha" -- "${paths[@]}"; then
  skip "nothing a release ships changed since $latest"
fi

IFS=. read -r major minor patch <<<"${latest#v}"
next="v$major.$minor.$((patch + 1))"
if git rev-parse -q --verify "refs/tags/$next" >/dev/null; then
  skip "$next already exists"
fi
echo "autorelease: $latest -> $next at $sha" >&2
echo "tag=$next"
