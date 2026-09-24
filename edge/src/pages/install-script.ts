/** GET /install.sh — the CLI installer (source of truth: scripts/install.sh, bundled as text). */
import script from "../../../scripts/install.sh";

export function installScript(): Response {
  return new Response(script, {
    headers: {
      "content-type": "text/x-shellscript; charset=utf-8",
      "cache-control": "public, max-age=300",
      "x-content-type-options": "nosniff",
    },
  });
}
