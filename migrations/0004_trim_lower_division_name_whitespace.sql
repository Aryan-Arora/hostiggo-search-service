-- Data hygiene fix: two `locations` rows had trailing whitespace in
-- lower_division_name (found via a full-schema sweep for whitespace/
-- non-ASCII anomalies across every identity/filter field, prompted by the
-- Haryāna typo fix in 0003).
--
-- location_id 34: "Parshavnath paramount society flat 101 t1 " (trailing space)
-- location_id 55: "Park royal Apartment "                      (trailing space)
--
-- Functionally harmless for search today — this field only feeds
-- to_tsvector(), and tsvector tokenization ignores whitespace, and it's
-- never used in an exact-match filter — but cleaned up anyway since it's a
-- zero-risk fix (confirmed no other row shares the trimmed value, so this
-- can't collide with the table's unique constraint on (state, district,
-- lower_division_name, lower_division_type, pincode)) and the value could
-- be surfaced to users directly in a UI at some point.

BEGIN;

UPDATE locations SET lower_division_name = trim(lower_division_name)
WHERE location_id IN (34, 55);

COMMIT;
