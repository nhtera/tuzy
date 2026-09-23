/**
 * Audit log. Rows are prepared statements that callers put in the SAME D1 batch as the change they
 * describe, so a committed change always has its audit row (and a rolled-back one never does).
 */
import { nowSec } from "./ids";

export interface AuditEntry {
  actor: string | null;
  action: string;
  target?: string | null;
  meta?: Record<string, unknown>;
  ipPrefix?: string | null;
}

const COLS = "INSERT INTO audit_log (id, actor_user_id, action, target, meta, ip_prefix, created_at)";
const VALS = "SELECT 'aud_' || lower(hex(randomblob(8))), ?1, ?2, ?3, ?4, ?5, ?6";

function bind(stmt: D1PreparedStatement, e: AuditEntry, extra: unknown[] = []): D1PreparedStatement {
  return stmt.bind(e.actor, e.action, e.target ?? null, e.meta ? JSON.stringify(e.meta) : null, e.ipPrefix ?? null, nowSec(), ...extra);
}

/** Unconditional audit row. */
export function audit(db: D1Database, e: AuditEntry): D1PreparedStatement {
  return bind(db.prepare(`${COLS} ${VALS}`), e);
}

/** Audit row written only if the statement right before it in the batch changed a row. */
export function auditIfChanged(db: D1Database, e: AuditEntry): D1PreparedStatement {
  return bind(db.prepare(`${COLS} ${VALS} WHERE changes() > 0`), e);
}

/** Audit row written only when `condition` (SQL using ?7…) holds at that point of the batch. */
export function auditWhen(db: D1Database, e: AuditEntry, condition: string, ...params: unknown[]): D1PreparedStatement {
  return bind(db.prepare(`${COLS} ${VALS} WHERE ${condition}`), e, params);
}
