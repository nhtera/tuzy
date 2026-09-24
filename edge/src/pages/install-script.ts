/**
 * GET /install.sh and /install.ps1 — the CLI installers (source of truth: scripts/install.sh and
 * scripts/install.ps1, bundled as text).
 */
import shScript from "../../../scripts/install.sh";
import ps1Script from "../../../scripts/install.ps1";

function serve(body: string, contentType: string): Response {
  return new Response(body, {
    headers: {
      "content-type": `${contentType}; charset=utf-8`,
      "cache-control": "public, max-age=300",
      "x-content-type-options": "nosniff",
    },
  });
}

export function installScript(): Response {
  return serve(shScript, "text/x-shellscript");
}

/** `irm https://tuzy.dev/install.ps1 | iex`: text/plain so Invoke-RestMethod returns a string. */
export function installPowerShell(): Response {
  return serve(ps1Script, "text/plain");
}
