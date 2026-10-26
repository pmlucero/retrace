-- Schema v2: gaps in the event log (spec §7.7, ADR 0010).
-- dropped_events counts the events the recorder assigned a seq to but did not
-- write. It is written when the session ends. Cassettes of schema v1 were all
-- written before any release, so they migrate with 0.
ALTER TABLE session ADD COLUMN dropped_events INTEGER NOT NULL DEFAULT 0;
