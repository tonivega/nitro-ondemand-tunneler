#!/usr/bin/env sh
set -eu
cd "$(dirname "$0")/.."
exec docker build --target artifacts --output type=local,dest=./output .
