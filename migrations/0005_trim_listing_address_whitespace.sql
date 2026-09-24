-- Continuation of the whitespace/non-ASCII sweep from 0004, extended to
-- listings' own address fields. Found via the same method: comparing each
-- value against trim(value).
--
-- Unlike locations.lower_division_name, `listings` has no unique constraint
-- touching these columns, so no pre-check for collisions is needed here.
--
-- Same "functionally harmless today, worth fixing anyway" reasoning as
-- 0004: these feed listings.search_vector (weight D) via to_tsvector(),
-- which already ignores whitespace when tokenizing, so no search behavior
-- changes — this is display/data hygiene, not a search bug fix.

BEGIN;

UPDATE listings
SET address_line1 = trim(address_line1)
WHERE address_line1 IS NOT NULL AND address_line1 != trim(address_line1);

UPDATE listings
SET address_line2 = trim(address_line2)
WHERE address_line2 IS NOT NULL AND address_line2 != trim(address_line2);

UPDATE listings
SET landmark = trim(landmark)
WHERE landmark IS NOT NULL AND landmark != trim(landmark);

COMMIT;
