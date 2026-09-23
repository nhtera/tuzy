/** Uniform API errors: `{ "error": { "code", "message", ...extra } }`. */

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
    readonly extra: Record<string, unknown> = {},
    readonly headers: Record<string, string> = {},
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function errorResponse(status: number, code: string, message: string, extra: Record<string, unknown> = {}, headers: Record<string, string> = {}): Response {
  return Response.json({ error: { code, message, ...extra } }, { status, headers: { "cache-control": "no-store", ...headers } });
}

export function toResponse(e: unknown): Response {
  if (e instanceof ApiError) return errorResponse(e.status, e.code, e.message, e.extra, e.headers);
  console.error("unhandled API error", e);
  return errorResponse(500, "internal", "internal error");
}

/** Parses a JSON object body or throws 400. */
export async function jsonBody(req: Request): Promise<Record<string, unknown>> {
  let v: unknown;
  try {
    v = await req.json();
  } catch {
    throw new ApiError(400, "bad_request", "request body must be JSON");
  }
  if (!v || typeof v !== "object" || Array.isArray(v)) throw new ApiError(400, "bad_request", "request body must be a JSON object");
  return v as Record<string, unknown>;
}
