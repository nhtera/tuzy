#!/usr/bin/env bash
# Refreshes edge/src/data/disposable-domains.json from the community blocklist.
set -euo pipefail
SRC="https://raw.githubusercontent.com/disposable-email-domains/disposable-email-domains/main/disposable_email_blocklist.conf"
OUT="$(cd "$(dirname "$0")/.." && pwd)/edge/src/data/disposable-domains.json"
curl -fsSL "$SRC" | tr -d '\r' | grep -v '^[[:space:]]*#' | grep -v '^[[:space:]]*$' | tr 'A-Z' 'a-z' | sort -u \
  | python3 -c 'import json,sys; json.dump([l.strip() for l in sys.stdin], sys.stdout, separators=(",",":"))' > "$OUT"
echo "wrote $(python3 -c "import json;print(len(json.load(open('$OUT'))))") domains to $OUT"
