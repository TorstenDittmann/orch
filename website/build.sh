#!/bin/sh
set -eu

site_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
cd "$site_dir"
bun run check
bun run build:docs
mkdir -p "$site_dir/dist/server"
cp "$site_dir/worker.js" "$site_dir/dist/server/index.js"
