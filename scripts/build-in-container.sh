#!/usr/bin/env sh
# Internal cross-compilation step, invoked by the Dockerfile.
set -eu
cd "$(dirname "$0")/.."
mkdir -p output
export CGO_ENABLED=0
unset GOOS GOARCH
for os in linux darwin windows; do
  for arch in amd64 arm64; do
    suffix=""
    [ "$os" != windows ] || suffix=".exe"
    echo "Building $os/$arch"
    GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags="-s -w" -o "output/nitro-ondemand-tunneler-$os-$arch$suffix" .
  done
done
go run ./scripts/checksums.go
