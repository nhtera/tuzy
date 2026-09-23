-- Phase 7: abuse reports, audit log, monthly long-stream usage.

CREATE TABLE abuse_reports (
  id               TEXT PRIMARY KEY,                               -- rpt_<16 hex>
  name             TEXT NOT NULL,
  category         TEXT NOT NULL CHECK (category IN ('phishing','malware','spam','illegal','other')),
  details          TEXT,
  reporter_email   TEXT,
  reporter_user_id TEXT,                                           -- logged-in reporter: weighted ×2
  reporter_prefix  TEXT NOT NULL,                                  -- IPv4 /24, IPv6 /48 (distinctness)
  owner_user_id    TEXT,                                           -- the name's owner when reported
  status           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open','actioned','dismissed')),
  alert_sent       INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL
);
CREATE INDEX idx_abuse_name_created ON abuse_reports(name, created_at);
CREATE INDEX idx_abuse_status_created ON abuse_reports(status, created_at);

CREATE TABLE audit_log (
  id            TEXT PRIMARY KEY,                                  -- aud_<16 hex>
  actor_user_id TEXT,                                              -- NULL = system (auto-quarantine, cron)
  action        TEXT NOT NULL,                                     -- name.reserve, token.revoke, admin.suspend_user …
  target        TEXT,
  meta          TEXT,                                              -- JSON
  ip_prefix     TEXT,
  created_at    INTEGER NOT NULL
);
CREATE INDEX idx_audit_created ON audit_log(created_at);

CREATE TABLE usage_monthly (
  user_id             TEXT NOT NULL,
  month               TEXT NOT NULL,                               -- UTC yyyy-mm
  long_stream_seconds INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, month)
);

-- One-shot markers so follow-up rows in the same batch fire only when THIS request made the change.
ALTER TABLE reservations ADD COLUMN change_ref TEXT;
ALTER TABLE users ADD COLUMN change_ref TEXT;
