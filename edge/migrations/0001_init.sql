-- Phase 4: accounts, email-OTP login, tokens, email budget, D1→DO outbox.

CREATE TABLE users (
  id            TEXT PRIMARY KEY,                                  -- usr_<16 hex>
  email         TEXT NOT NULL UNIQUE,                              -- trimmed + lowercased
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended','deleted')),
  role          TEXT NOT NULL DEFAULT 'user'   CHECK (role IN ('user','admin')),
  trusted       INTEGER NOT NULL DEFAULT 0,                        -- admin-granted trust (phase 7)
  trust_revoked INTEGER NOT NULL DEFAULT 0,                        -- sticky: upheld abuse report
  max_names     INTEGER NOT NULL DEFAULT 10,                       -- pinned-name quota (phase 5)
  created_at    INTEGER NOT NULL,                                  -- unix seconds
  last_login_at INTEGER,
  deleted_at    INTEGER                                            -- soft delete; email erased 30 d later
);

CREATE TABLE tokens (
  id           TEXT PRIMARY KEY,                                   -- tok_<16 hex>
  user_id      TEXT NOT NULL REFERENCES users(id),
  token_hash   TEXT NOT NULL UNIQUE,                               -- sha256 hex of the full token
  scope        TEXT NOT NULL DEFAULT 'full' CHECK (scope IN ('full','connect')),
  label        TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,                                            -- optional absolute expiry (CI tokens)
  last_used_at INTEGER,                                            -- every token idle-expires after 90 d
  revoked_at   INTEGER
);
CREATE INDEX idx_tokens_user ON tokens(user_id);

CREATE TABLE login_codes (
  id             TEXT PRIMARY KEY,                                 -- login_id (128-bit hex)
  email          TEXT NOT NULL,                                    -- exact address the code was sent to
  mailbox        TEXT NOT NULL,                                    -- canonical mailbox (caps): no +tag, no gmail dots
  purpose        TEXT NOT NULL DEFAULT 'login' CHECK (purpose IN ('login','delete')),
  code_hash      TEXT NOT NULL,                                    -- sha256(id || ':' || code)
  attempts       INTEGER NOT NULL DEFAULT 0,
  ip_prefix      TEXT NOT NULL,                                    -- IPv4 /32, IPv6 /64
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL,                                 -- created_at + 600
  last_sent_at   INTEGER NOT NULL,
  send_failed_at INTEGER,                                          -- kept: still counts toward caps
  consumed_at    INTEGER
);
CREATE INDEX idx_login_codes_email_created   ON login_codes(email, created_at);
CREATE INDEX idx_login_codes_mailbox_created ON login_codes(mailbox, created_at);
CREATE INDEX idx_login_codes_prefix_created ON login_codes(ip_prefix, created_at);

CREATE TABLE email_budget (
  day  TEXT PRIMARY KEY,                                           -- UTC yyyy-mm-dd
  sent INTEGER NOT NULL DEFAULT 0
);

-- Which token has connected which name (one row per name × token, written after the DO accepted
-- the connect, never on visitor traffic) so token revocation / account deletion reach the right DOs.
CREATE TABLE tunnel_sessions (
  name         TEXT NOT NULL,
  token_id     TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  connected_at INTEGER NOT NULL,
  PRIMARY KEY (name, token_id)
);
CREATE INDEX idx_tunnel_sessions_user  ON tunnel_sessions(user_id);
CREATE INDEX idx_tunnel_sessions_token ON tunnel_sessions(token_id);

-- D1 → DO pushes that must converge (revoke, rename, suspend…): written in the same batch as the
-- state change, pushed immediately, retried by the */5 cron.
CREATE TABLE do_outbox (
  id         TEXT PRIMARY KEY,                                     -- obx_<16 hex>
  name       TEXT NOT NULL,
  action     TEXT NOT NULL,
  payload    TEXT NOT NULL,
  attempts   INTEGER NOT NULL DEFAULT 0,
  next_at    INTEGER NOT NULL,
  done_at    INTEGER,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_outbox_pending ON do_outbox(done_at, next_at);
