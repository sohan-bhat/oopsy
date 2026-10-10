#!/bin/sh
# Builds release archives with GoReleaser and publishes them with gh.
# Usage: scripts/release.sh v0.1.0
set -eu
tag="${1:?usage: scripts/release.sh vX.Y.Z}"
cd "$(dirname "$0")/.."
git tag -a "$tag" -m "$tag" 2>/dev/null || true
git push origin "$tag"
goreleaser release --clean --skip=publish,validate
gh release create "$tag" dist/oopsy_*.tar.gz dist/checksums.txt --title "$tag" --generate-notes
echo "Now update Formula/oopsy.rb in sohan-bhat/homebrew-tap with the new sha256 values from dist/checksums.txt"
