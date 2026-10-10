#!/bin/sh
# Builds release archives with GoReleaser and publishes them with gh.
# Usage: scripts/release.sh v0.2.0
set -eu
tag="${1:?usage: scripts/release.sh vX.Y.Z}"
cd "$(dirname "$0")/.."
git tag -a "$tag" -m "$tag" 2>/dev/null || true
git push origin "$tag"
goreleaser release --clean --skip=publish,validate
gh release create "$tag" dist/oopsy_*.tar.gz dist/oopsy_*.zip dist/checksums.txt --title "$tag" --notes-file "${NOTES:-/dev/null}"
echo "Now update Formula/oopsy.rb in sohan-bhat/homebrew-tap and bucket/oopsy.json in"
echo "sohan-bhat/scoop-bucket with the new sha256 values from dist/checksums.txt."
