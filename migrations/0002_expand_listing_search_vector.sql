-- Expands listings.search_vector to cover location/address and amenities,
-- not just title/description/property-type/stay-type (0001's original
-- scope). Still pure lexical tsvector/tsquery — no pg_trgm, no fuzzy
-- matching added here.
--
-- Weighting:
--   A: title
--   B: description
--   C: property type, stay type, district, state, lower_division_name
--      (facets that identify *what* and *where* the listing broadly is)
--   D: address_line1/2, landmark, amenity names
--      (supplementary detail — shouldn't outrank a real title/description
--      match, but should still surface a listing when someone searches a
--      neighborhood, landmark, or amenity by name)
--
-- Run against schema hostiggo_testing_schema.

BEGIN;

-- Single canonical computation, reused by the row-level trigger's backfill
-- path and by the listing_amenities trigger (amenities live in a different
-- table, so a listings-only trigger can't see when they change).
CREATE OR REPLACE FUNCTION refresh_listing_search_vector(p_listing_id integer) RETURNS void AS $$
BEGIN
  UPDATE listings l
  SET search_vector = sub.vec
  FROM (
    SELECT
      l2.listing_id,
      setweight(to_tsvector('english', coalesce(l2.title, '')), 'A') ||
      setweight(to_tsvector('english', coalesce(l2.description, '')), 'B') ||
      setweight(to_tsvector('english',
        coalesce(pt.name, '') || ' ' || coalesce(st.title, '') || ' ' ||
        coalesce(loc.district, '') || ' ' || coalesce(loc.state, '') || ' ' ||
        coalesce(loc.lower_division_name, '')
      ), 'C') ||
      setweight(to_tsvector('english',
        coalesce(l2.address_line1, '') || ' ' || coalesce(l2.address_line2, '') || ' ' ||
        coalesce(l2.landmark, '') || ' ' || coalesce(am.amenity_names, '')
      ), 'D') AS vec
    FROM listings l2
    LEFT JOIN property_types pt ON pt.id = l2.property_type_id
    LEFT JOIN stay_types st ON st.id = l2.stay_type_id
    LEFT JOIN locations loc ON loc.location_id = l2.location_id
    LEFT JOIN LATERAL (
      SELECT string_agg(a.name, ' ') AS amenity_names
      FROM listing_amenities la JOIN amenities a ON a.amenity_id = la.amenity_id
      WHERE la.listing_id = l2.listing_id
    ) am ON TRUE
    WHERE l2.listing_id = p_listing_id
  ) sub
  WHERE l.listing_id = sub.listing_id;
END;
$$ LANGUAGE plpgsql;

-- Replace the listings trigger: fires AFTER (not BEFORE) so the row-level
-- function above can SELECT the row's own just-written values, and only on
-- columns that actually feed the vector (an UPDATE that touches only
-- search_vector itself won't re-trigger — no infinite loop).
DROP TRIGGER IF EXISTS trg_listings_search_vector ON listings;
DROP FUNCTION IF EXISTS listings_search_vector_update();

CREATE OR REPLACE FUNCTION listings_search_vector_trigger() RETURNS trigger AS $$
BEGIN
  PERFORM refresh_listing_search_vector(NEW.listing_id);
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_listings_search_vector
  AFTER INSERT OR UPDATE OF title, description, property_type_id, stay_type_id,
    location_id, address_line1, address_line2, landmark
  ON listings
  FOR EACH ROW
  EXECUTE FUNCTION listings_search_vector_trigger();

-- New: refresh the owning listing's vector whenever its amenities change.
CREATE OR REPLACE FUNCTION listing_amenities_search_vector_trigger() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    PERFORM refresh_listing_search_vector(OLD.listing_id);
    RETURN OLD;
  ELSE
    PERFORM refresh_listing_search_vector(NEW.listing_id);
    RETURN NEW;
  END IF;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_listing_amenities_search_vector ON listing_amenities;
CREATE TRIGGER trg_listing_amenities_search_vector
  AFTER INSERT OR DELETE ON listing_amenities
  FOR EACH ROW
  EXECUTE FUNCTION listing_amenities_search_vector_trigger();

-- Backfill all existing rows with the expanded vector (bulk, not per-row
-- function calls, for speed).
UPDATE listings l
SET search_vector = sub.vec
FROM (
  SELECT
    l2.listing_id,
    setweight(to_tsvector('english', coalesce(l2.title, '')), 'A') ||
    setweight(to_tsvector('english', coalesce(l2.description, '')), 'B') ||
    setweight(to_tsvector('english',
      coalesce(pt.name, '') || ' ' || coalesce(st.title, '') || ' ' ||
      coalesce(loc.district, '') || ' ' || coalesce(loc.state, '') || ' ' ||
      coalesce(loc.lower_division_name, '')
    ), 'C') ||
    setweight(to_tsvector('english',
      coalesce(l2.address_line1, '') || ' ' || coalesce(l2.address_line2, '') || ' ' ||
      coalesce(l2.landmark, '') || ' ' || coalesce(am.amenity_names, '')
    ), 'D') AS vec
  FROM listings l2
  LEFT JOIN property_types pt ON pt.id = l2.property_type_id
  LEFT JOIN stay_types st ON st.id = l2.stay_type_id
  LEFT JOIN locations loc ON loc.location_id = l2.location_id
  LEFT JOIN LATERAL (
    SELECT string_agg(a.name, ' ') AS amenity_names
    FROM listing_amenities la JOIN amenities a ON a.amenity_id = la.amenity_id
    WHERE la.listing_id = l2.listing_id
  ) am ON TRUE
) sub
WHERE l.listing_id = sub.listing_id;

-- Known limitation: if a `locations` row's own state/district/
-- lower_division_name text is edited later, listings referencing it are NOT
-- automatically re-indexed (locations changes are rare/administrative; a
-- cascading trigger wasn't worth the complexity here). Re-run
-- refresh_listing_search_vector() for affected listing_ids after such an
-- edit, or re-run this migration's backfill UPDATE.

COMMIT;
