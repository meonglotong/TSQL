#!/usr/bin/env bash
# install.sh — quick local install of TSQL on a RHEL-style system.
#
# Builds tsqld + tsql from source (needs a Go toolchain), installs them to
# /usr/local/bin, creates the tsql user and /var/lib/tsql, installs the
# systemd unit (ExecStart rewritten to /usr/local/bin), and starts the
# service. For the dnf/rpm path, use packaging/tsql.spec instead.
#
# Usage: sudo ./install.sh [source-dir]   (default: repo root next to this file)
set -euo pipefail

SRC="${1:-$(cd "$(dirname "$0")" && pwd)}"
PREFIX=/usr/local
DATADIR=/var/lib/tsql
UNIT_SRC="$SRC/packaging/systemd/tsqld.service"
UNIT_DST=/usr/lib/systemd/system/tsqld.service

if [ "$(id -u)" -ne 0 ]; then
  echo "install: must be run as root" >&2
  exit 1
fi
command -v go >/dev/null 2>&1 || { echo "install: go toolchain not found in PATH" >&2; exit 1; }
[ -f "$UNIT_SRC" ] || { echo "install: missing $UNIT_SRC" >&2; exit 1; }

echo "==> Building tsqld + tsql from $SRC"
( cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/tsqld ./cmd/tsqld )
( cd "$SRC" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o bin/tsql ./cmd/tsql )

echo "==> Installing binaries to $PREFIX/bin"
install -d -m 0755 "$PREFIX/bin"
install -m 0755 "$SRC/bin/tsqld" "$PREFIX/bin/tsqld"
install -m 0755 "$SRC/bin/tsql" "$PREFIX/bin/tsql"

echo "==> Creating tsql user and datadir"
getent group tsql >/dev/null 2>&1 || groupadd -r tsql
getent passwd tsql >/dev/null 2>&1 || useradd -r -g tsql -d "$DATADIR" -s /sbin/nologin tsql
install -d -o tsql -g tsql -m 0750 "$DATADIR"

echo "==> Installing systemd unit (ExecStart -> $PREFIX/bin/tsqld)"
install -d -m 0755 "$(dirname "$UNIT_DST")"
sed "s|/usr/bin/tsqld|$PREFIX/bin/tsqld|g" "$UNIT_SRC" > "$UNIT_DST"
chmod 0644 "$UNIT_DST"

if [ -d /run/systemd/system ]; then
  systemctl daemon-reload
  systemctl enable --now tsqld.service
else
  echo "install: systemd not running; start tsqld manually with:"
  echo "  $PREFIX/bin/tsqld -datadir $DATADIR -listen 127.0.0.1:5433 -unixsocket /run/tsql/tsqld.sock"
fi

echo "==> Done. Try:  tsql -c \"SELECT NOW()\""
