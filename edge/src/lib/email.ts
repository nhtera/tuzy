/**
 * Outbound email. Drivers:
 *   cloudflare — Email Service `send_email` binding (README "GATE A")
 *   log        — dev/tests: records messages in memory (`sentEmails`) instead of sending
 * Any failure surfaces as EmailUnavailableError → 503 `email_unavailable`.
 */

export interface OutboundEmail {
  to: string;
  subject: string;
  text: string;
}

export class EmailUnavailableError extends Error {
  constructor(cause?: unknown) {
    super("email is temporarily unavailable");
    this.name = "EmailUnavailableError";
    this.cause = cause;
  }
}

/** Messages captured by the `log` driver (tests read the codes from here). */
export const sentEmails: OutboundEmail[] = [];

export async function sendEmail(env: Env, msg: OutboundEmail): Promise<void> {
  if (env.EMAIL_DRIVER === "log") {
    // The log driver never sends: refuse it unless explicitly allowed (tests / local dev).
    if (env.ALLOW_LOG_EMAIL !== "1") throw new EmailUnavailableError("EMAIL_DRIVER=log without ALLOW_LOG_EMAIL=1");
    // Dev/tests only: "+sendfail@" recipients simulate a provider failure.
    if (msg.to.includes("+sendfail@")) throw new EmailUnavailableError("simulated failure");
    sentEmails.push(msg);
    return;
  }
  if (!env.EMAIL) throw new EmailUnavailableError("EMAIL binding missing");
  try {
    await env.EMAIL.send({ to: msg.to, from: { email: env.EMAIL_FROM, name: "Tuzy" }, subject: msg.subject, text: msg.text });
  } catch (e) {
    throw new EmailUnavailableError(e);
  }
}

export function loginCodeEmail(to: string, code: string, purpose: "login" | "delete"): OutboundEmail {
  const pretty = `${code.slice(0, 3)} ${code.slice(3)}`;
  if (purpose === "delete") {
    return {
      to,
      subject: `Confirm deleting your Tuzy account: ${pretty}`,
      text: `Your Tuzy account deletion code is ${pretty}.\n\nIt expires in 10 minutes. Entering it permanently deletes your account, revokes all tokens and closes your tunnels.\n\nDidn't request this? Ignore this email; nothing changes.\n`,
    };
  }
  return {
    to,
    subject: `Your Tuzy login code: ${pretty}`,
    text: `Your Tuzy login code is ${pretty}.\n\nIt expires in 10 minutes.\n\nDidn't request this? Ignore this email; nobody can log in without the code.\n`,
  };
}
