#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BIN_DIR="${HOME}/.local/bin"

mkdir -p "$BIN_DIR"

cp -f "$SCRIPT_DIR/main.rb" "$BIN_DIR/dot"
chmod +x "$BIN_DIR/dot"

printf 'installed dot to %s\n' "$BIN_DIR/dot"
