#!/bin/sh
# DevKuong Memories installer.
#
# This is the first code a stranger runs on their machine because a README told
# them to. It is therefore kept short enough to read in one screen, it does
# nothing that is not printed, and it touches exactly two things: one binary and
# (only if asked) one line in a shell profile.
#
#   curl -fsSL https://raw.githubusercontent.com/IzE-PewPewPew/DK-AgentMemory/main/scripts/install.sh | sh
#
# Read it first if you like — that is the point of it being this short:
#   curl -fsSL .../install.sh -o install.sh && less install.sh && sh install.sh

set -eu

REPO="IzE-PewPewPew/DK-AgentMemory"
INSTALL_DIR="${DKM_INSTALL_DIR:-$HOME/.local/bin}"
VERSION="${DKM_VERSION:-latest}"

say()  { printf '%s\n' "$*"; }
die()  { printf 'install: %s\n' "$*" >&2; exit 1; }

# --- platform ---------------------------------------------------------------

os=$(uname -s)
case "$os" in
  Linux)  goos=linux  ;;
  Darwin) goos=darwin ;;
  *) die "unsupported operating system: $os. Download a binary from https://github.com/$REPO/releases" ;;
esac

arch=$(uname -m)
case "$arch" in
  x86_64|amd64)  goarch=amd64 ;;
  arm64|aarch64) goarch=arm64 ;;
  *) die "unsupported architecture: $arch" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
command -v tar  >/dev/null 2>&1 || die "tar is required"

# --- resolve the version ----------------------------------------------------

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$VERSION" ] || die "could not determine the latest release. Set DKM_VERSION=vX.Y.Z"
fi

ASSET="dkm_${goos}_${goarch}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

say "DevKuong Memories $VERSION  ($goos/$goarch)"
say ""

# --- download and verify ----------------------------------------------------

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

say "  downloading $ASSET"
curl -fsSL "$BASE/$ASSET" -o "$tmp/$ASSET" \
  || die "download failed. Check https://github.com/$REPO/releases/tag/$VERSION"

# Checksums are verified when the tools to do it are present. Skipping silently
# would defeat the point, so a skip is announced.
if curl -fsSL "$BASE/dkm_checksums.txt" -o "$tmp/checksums.txt" 2>/dev/null; then
  if command -v sha256sum >/dev/null 2>&1; then
    ( cd "$tmp" && sha256sum -c checksums.txt --ignore-missing >/dev/null 2>&1 ) \
      || die "checksum mismatch — the download is corrupt or tampered with"
    say "  checksum verified"
  elif command -v shasum >/dev/null 2>&1; then
    ( cd "$tmp" && shasum -a 256 -c checksums.txt --ignore-missing >/dev/null 2>&1 ) \
      || die "checksum mismatch — the download is corrupt or tampered with"
    say "  checksum verified"
  else
    say "  checksum NOT verified (no sha256sum or shasum on this machine)"
  fi
else
  say "  checksum NOT verified (checksum file unavailable)"
fi

tar -xzf "$tmp/$ASSET" -C "$tmp"
[ -f "$tmp/dkm" ] || die "the archive did not contain a dkm binary"

# --- install ----------------------------------------------------------------

mkdir -p "$INSTALL_DIR"
mv "$tmp/dkm" "$INSTALL_DIR/dkm"
chmod 755 "$INSTALL_DIR/dkm"

say "  installed $INSTALL_DIR/dkm"

# --- PATH -------------------------------------------------------------------

case ":$PATH:" in
  *":$INSTALL_DIR:"*)
    on_path=yes ;;
  *)
    on_path=no ;;
esac

if [ "$on_path" = no ]; then
  say ""
  say "  $INSTALL_DIR is not on your PATH. Add it:"
  say ""
  say "      export PATH=\"\$PATH:$INSTALL_DIR\""
  say ""
  say "  Put that line in your shell profile to make it permanent."
  say "  (This installer does not edit shell profiles.)"
fi

say ""
"$INSTALL_DIR/dkm" version
say ""
say "Next:"
say "    dkm login https://your-server"
say "    dkm connect --all"
say "    dkm doctor"
