#!/bin/sh
# tuzy installer: curl -fsSL https://tuzy.dev/install.sh | sh
#
# Downloads the tuzy release for this OS/arch from GitHub, verifies checksums.txt (ed25519
# signature with the tuzy release key when openssl can check it, always sha256) and installs the
# binary to $TUZY_INSTALL_DIR (default ~/.local/bin). Pin a version with TUZY_VERSION=v1.2.3.
# Windows: use `scoop bucket add nhtera https://github.com/nhtera/scoop-bucket; scoop install tuzy`.
set -eu

REPO="https://github.com/nhtera/tuzy/releases"
# SubjectPublicKeyInfo (DER, base64) of the tuzy release key — same key as internal/update/keys.go.
RELEASE_KEY_DER="MCowBQYDK2VwAyEAtQiuluMwH12IIzLX4kVjYKu/+DKbRpKpa/Rn/pCyd5o="

# Test hook (scripts/install_test.sh): a local release mirror and its throwaway key, together only.
if [ -n "${TUZY_TEST_RELEASES_URL:-}" ]; then
  REPO="$TUZY_TEST_RELEASES_URL"
  RELEASE_KEY_DER="${TUZY_TEST_RELEASE_KEY_DER:?}"
fi
proto="=https"
case "$REPO" in http://127.0.0.1:*) proto="=http" ;; esac

say() { printf '%s\n' "$*" >&2; }
die() { say "tuzy install: $*"; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

# Everything runs inside main: a truncated download (curl | sh) executes nothing.
main() {
  if [ "$(id -u)" = 0 ] && [ -z "${TUZY_INSTALL_DIR:-}" ]; then
    die "don't run as root; to install system-wide set TUZY_INSTALL_DIR=/usr/local/bin explicitly"
  fi
  need curl
  need tar
  need uname

  os=$(uname -s)
  case "$os" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) die "unsupported OS $os (on Windows use scoop; elsewhere: go install github.com/nhtera/tuzy/cmd/tuzy@latest)" ;;
  esac
  arch=$(uname -m)
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) die "unsupported architecture $arch" ;;
  esac

  version="${TUZY_VERSION:-}"
  if [ -z "$version" ]; then
    # The first redirect of releases/latest names the newest stable tag (no API rate limit).
    loc=$(curl -fsS -o /dev/null -w '%{redirect_url}' "$REPO/latest/download/checksums.txt") || die "cannot reach GitHub"
    version=$(printf '%s' "$loc" | sed -n 's#.*/releases/download/\(v[^/]*\)/.*#\1#p')
    [ -n "$version" ] || die "no published release found"
  fi
  case "$version" in v*) ;; *) version="v$version" ;; esac

  asset="tuzy_${version#v}_${os}_${arch}.tar.gz"
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT INT TERM
  say "Downloading tuzy ${version} for ${os}/${arch}..."
  for f in checksums.txt checksums.txt.sig "$asset"; do
    curl -fsSL --proto "$proto" -o "$tmp/$f" "$REPO/download/$version/$f" || die "download failed: $f"
  done

  # 1) Signature (when this openssl can verify ed25519; LibreSSL on older macOS can't).
  printf '%s' "$RELEASE_KEY_DER" | base64 -d >"$tmp/key.der" 2>/dev/null || printf '%s' "$RELEASE_KEY_DER" | base64 -D >"$tmp/key.der"
  base64 -d <"$tmp/checksums.txt.sig" >"$tmp/sig.bin" 2>/dev/null || base64 -D <"$tmp/checksums.txt.sig" >"$tmp/sig.bin"
  if command -v openssl >/dev/null 2>&1 &&
    openssl pkey -pubin -inform DER -in "$tmp/key.der" -out "$tmp/key.pem" >/dev/null 2>&1; then
    if openssl pkeyutl -verify -pubin -inkey "$tmp/key.pem" -rawin -in "$tmp/checksums.txt" -sigfile "$tmp/sig.bin" >/dev/null 2>&1; then
      say "✓ signature verified"
    elif openssl pkeyutl -help 2>&1 | grep -q -- '-rawin'; then
      die "checksums.txt signature does NOT match the tuzy release key"
    else
      say "note: this openssl can't verify ed25519; relying on HTTPS + sha256"
    fi
  else
    say "note: openssl unavailable for signature checks; relying on HTTPS + sha256"
  fi

  # 2) sha256 of the archive.
  want=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1 }' "$tmp/checksums.txt")
  [ -n "$want" ] || die "$asset is not listed in checksums.txt"
  if command -v sha256sum >/dev/null 2>&1; then
    got=$(sha256sum "$tmp/$asset" | awk '{print $1}')
  else
    got=$(shasum -a 256 "$tmp/$asset" | awk '{print $1}')
  fi
  [ "$got" = "$want" ] || die "sha256 mismatch for $asset"
  say "✓ checksum verified"

  tar -xzf "$tmp/$asset" -C "$tmp" tuzy || die "archive has no tuzy binary"
  dir="${TUZY_INSTALL_DIR:-$HOME/.local/bin}"
  mkdir -p "$dir" || die "cannot create $dir (set TUZY_INSTALL_DIR)"
  # Same-directory temp + rename: an existing tuzy is replaced atomically.
  if ! { cp "$tmp/tuzy" "$dir/.tuzy-install.$$" && chmod 755 "$dir/.tuzy-install.$$" && mv -f "$dir/.tuzy-install.$$" "$dir/tuzy"; }; then
    rm -f "$dir/.tuzy-install.$$"
    die "cannot write to $dir (set TUZY_INSTALL_DIR, or run with sudo for a system directory)"
  fi
  "$dir/tuzy" version >/dev/null 2>&1 || die "installed binary doesn't run on this system"

  say "✓ installed $("$dir/tuzy" version) to $dir/tuzy"
  case ":$PATH:" in
    *":$dir:"*) ;;
    *) say "Add it to your PATH:  export PATH=\"$dir:\$PATH\"" ;;
  esac
  say "Next: tuzy login && tuzy http 3000"
}

main "$@"
