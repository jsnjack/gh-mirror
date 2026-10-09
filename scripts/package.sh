#!/usr/bin/env bash
set -euo pipefail
release_version=${1:?release version is required}
if [[ ! "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'Release version must be major.minor.patch' >&2
  exit 1
fi
mkdir -p dist
release_stage=$(mktemp -d)
trap 'rm -rf "$release_stage"' EXIT
for release_os in linux darwin; do
  for release_arch in amd64 arm64; do
    echo "Building $release_os/$release_arch" >&2
    CGO_ENABLED=0 GOOS="$release_os" GOARCH="$release_arch" \
      go build -trimpath -ldflags="-X gh-mirror/cmd.Version=$release_version" -o "$release_stage/gh-mirror" .
    tar -czf "dist/gh-mirror_${release_os}_${release_arch}.tar.gz" -C "$release_stage" gh-mirror -C "$PWD" README.md internal/embedding/NOTICE internal/embedding/assets/LICENSE
  done
done
(cd dist && sha256sum gh-mirror_{linux,darwin}_{amd64,arm64}.tar.gz > checksums.txt)
