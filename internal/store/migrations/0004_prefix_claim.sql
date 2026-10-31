-- `yuuka prefix claim` (§3.3): an address is claimed when it is handed
-- out, which is usually before any mail reaches it. first_seen/last_seen
-- are observations (§4.2), so a prefix nobody has delivered to has none:
-- NULL, not the time of the claim. ingest widens them with least() and
-- greatest(), which skip NULL, so the first delivery fills both.

ALTER TABLE prefixes
    ALTER COLUMN first_seen DROP NOT NULL,
    ALTER COLUMN last_seen DROP NOT NULL,
    ADD CONSTRAINT prefixes_seen_together CHECK ((first_seen IS NULL) = (last_seen IS NULL));
