-- Phase 5: pinned names. D1 is the source of truth; every mutation is ownership-guarded.

CREATE TABLE reservations (
  name         TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL REFERENCES users(id),               -- no cascade: users are soft-deleted
  gen          TEXT NOT NULL,                                     -- random per (name, owner) incarnation; passed to the DO
  is_default   INTEGER NOT NULL DEFAULT 0,
  status       TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','suspended')),
  last_seen_at INTEGER,                                           -- throttled (≤ 1/h)
  created_at   INTEGER NOT NULL
);
CREATE INDEX idx_reservations_user ON reservations(user_id);
CREATE UNIQUE INDEX idx_reservations_one_default ON reservations(user_id) WHERE is_default = 1;

CREATE TABLE released_names (
  name       TEXT PRIMARY KEY,
  held_for   TEXT NOT NULL,                                       -- previous owner, or '__blocked__' (admin block)
  until      INTEGER NOT NULL,                                    -- released_at + 365 d
  renamed_to TEXT                                                 -- set on rename: 410 hint
);
CREATE INDEX idx_released_names_held ON released_names(held_for, until);

-- Backfill: names used before reservations existed stay held (12 months) for their most recent
-- user, who can reclaim them with `tuzy names add`; nobody else can take their webhooks.
INSERT OR IGNORE INTO released_names (name, held_for, until, renamed_to)
SELECT ts.name, ts.user_id, CAST(strftime('%s', 'now') AS INTEGER) + 31536000, NULL
  FROM tunnel_sessions ts
 WHERE ts.connected_at = (SELECT MAX(connected_at) FROM tunnel_sessions WHERE name = ts.name);
