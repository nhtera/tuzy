#!/bin/sh
# Tests scripts/install.sh against a local mirror of a GoReleaser snapshot (dist/) signed with a
# throwaway key: good install, tampered archive, bad signature.
#   goreleaser release --snapshot --clean --skip=sign && sh scripts/install_test.sh
set -eu
root=$(cd "$(dirname "$0")/.." && pwd)
dist="$root/dist"
[ -f "$dist/checksums.txt" ] || { echo "run a goreleaser snapshot first" >&2; exit 1; }
work=$(mktemp -d)
port=${TUZY_TEST_PORT:-8791}
if curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1; then echo "port $port is busy; stop its owner or set TUZY_TEST_PORT" >&2; exit 1; fi
pid=""
cleanup() {
  if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
  rm -rf "$work"
}
trap cleanup EXIT INT TERM

version=$(sed -n 's/^.*tuzy_\([^_]*\)_[a-z]*_[a-z0-9]*\.tar\.gz$/\1/p' "$dist/checksums.txt" | head -1)
rel="$work/releases/download/v$version"
mkdir -p "$rel"
cp "$dist/checksums.txt" "$dist"/tuzy_*.tar.gz "$rel/"

pub=$(cd "$root" && go run ./tools/release-sign keygen "$work/key")
TUZY_RELEASE_KEY=$(cat "$work/key") && export TUZY_RELEASE_KEY
(cd "$root" && go run ./tools/release-sign sign "$rel/checksums.txt")
der=$( (printf '\060\052\060\005\006\003\053\145\160\003\041\000'; printf '%s' "$pub" | base64 -d 2>/dev/null || printf '%s' "$pub" | base64 -D) | base64 | tr -d '\n')

python3 -m http.server "$port" --bind 127.0.0.1 --directory "$work" >/dev/null 2>&1 &
pid=$! # the server itself (not a subshell), so cleanup really stops it
i=0
until curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1; do i=$((i + 1)); [ $i -lt 50 ] || exit 1; sleep 0.1; done

run() {
  TUZY_TEST_RELEASES_URL="http://127.0.0.1:$port/releases" TUZY_TEST_RELEASE_KEY_DER="$der" \
    TUZY_VERSION="v$version" TUZY_INSTALL_DIR="$work/bin" sh "$root/scripts/install.sh"
}

echo "== good install"
run || { echo "FAIL good install"; exit 1; }
"$work/bin/tuzy" version | grep -q "$version" && echo "PASS good install"

echo "== tampered archive"
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(find "$rel" -name "tuzy_*_${os}_*.tar.gz" | head -1)
cp "$arch" "$work/orig" && printf 'x' >>"$arch"
if run 2>"$work/err"; then echo "FAIL tampered archive installed"; exit 1; fi
grep -q "sha256 mismatch" "$work/err" && echo "PASS tampered archive refused"
cp "$work/orig" "$arch"

echo "== forged signature"
printf 'forged\n' >>"$rel/checksums.txt"
if run 2>"$work/err"; then
  if grep -q "can't verify ed25519\|unavailable" "$work/err"; then
    echo "SKIP forged signature (no ed25519 in this openssl)"
  else
    echo "FAIL forged checksums accepted"
    exit 1
  fi
else
  grep -q "does NOT match" "$work/err" && echo "PASS forged signature refused"
fi
