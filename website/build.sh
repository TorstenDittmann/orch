#!/bin/sh
set -eu

site_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
mkdir -p "$site_dir/dist/server"
cp "$site_dir/index.html" "$site_dir/dist/index.html"
cp "$site_dir/styles.css" "$site_dir/dist/styles.css"
cp "$site_dir/worker.js" "$site_dir/dist/server/index.js"
