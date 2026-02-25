#!/bin/sh

set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BIN_DIR="${HOME}/.local/bin"

mkdir -p "$BIN_DIR"

cd "$SCRIPT_DIR"
printf 'building dot...\n'
go build -o dot .
mv -f dot "$BIN_DIR/dot"

printf 'installed dot to %s\n' "$BIN_DIR/dot"
