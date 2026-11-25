-- PostgreSQL twin of migrations/sqlite/0012_review_fixes.sql.
-- ops_checkin_state.last_at: absolute instant (epoch ms) of the last check-in.
-- "Already checked in today" and streak continuity compare the local dates of
-- last_at and now, both in the currently configured timezone, so an admin
-- timezone change cannot open a second claim inside the same real day. 0 for
-- rows written before this migration (last_day is used for those).
ALTER TABLE ops_checkin_state ADD COLUMN last_at BIGINT NOT NULL DEFAULT 0;

-- invite_attributions.rewarded_at: when the inviter/invitee grants were paid
-- (epoch ms); 0 while they are pending because the invitee had no verified
-- platform identity at accept time. Earlier attributions were paid at accept.
ALTER TABLE invite_attributions ADD COLUMN rewarded_at BIGINT NOT NULL DEFAULT 0;
UPDATE invite_attributions SET rewarded_at = created_at;

-- match_rooms: the claiming API key and SHA-256 of the random room token issued
-- at claim. Battle calls must present both; epoch only orders claims.
ALTER TABLE match_rooms ADD COLUMN claim_key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE match_rooms ADD COLUMN token_hash TEXT NOT NULL DEFAULT '';
