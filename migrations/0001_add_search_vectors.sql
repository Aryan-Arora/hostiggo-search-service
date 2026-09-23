-- Adds persisted, GIN-indexed tsvector columns for lexical full-text search
-- (no pg_trgm, no fuzzy/similarity/edit-distance matching anywhere).
--
-- Per the schema audit: neither `listings` nor `locations` had any GIN,
-- pg_trgm, tsvector, or pgvector index. The only existing full-text usage
-- (search_locations_partial) computed to_tsvector() on the fly on every
-- call with no persisted column and no index backing it. This migration
-- fixes that gap and extends the same approach to `listings`.
--
-- Run against schema hostiggo_testing_schema (adjust search_path or qualify
-- table names if deploying against a different schema name).

BEGIN;

-- listings: title (weight A) + description (weight B) + joined property/stay
-- type names (weight C), refreshed automatically via a trigger since the
-- weighted expression spans a join and can't be a single-table generated
-- column.
ALTER TABLE listings ADD COLUMN IF NOT EXISTS search_vector tsvector;

CREATE OR REPLACE FUNCTION listings_search_vector_update() RETURNS trigger AS $$
DECLARE
  v_property_type_name text;
  v_stay_type_title text;
BEGIN
  SELECT pt.name INTO v_property_type_name FROM property_types pt WHERE pt.id = NEW.property_type_id;
  SELECT st.title INTO v_stay_type_title FROM stay_types st WHERE st.id = NEW.stay_type_id;

  NEW.search_vector :=
    setweight(to_tsvector('english', coalesce(NEW.title, '')), 'A') ||
    setweight(to_tsvector('english', coalesce(NEW.description, '')), 'B') ||
    setweight(to_tsvector('english', coalesce(v_property_type_name, '') || ' ' || coalesce(v_stay_type_title, '')), 'C');

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_listings_search_vector ON listings;
CREATE TRIGGER trg_listings_search_vector
  BEFORE INSERT OR UPDATE OF title, description, property_type_id, stay_type_id
  ON listings
  FOR EACH ROW
  EXECUTE FUNCTION listings_search_vector_update();

-- Backfill existing rows (228 at time of audit — cheap one-off UPDATE).
-- The trigger only fires when title/description/property_type_id/stay_type_id
-- actually change, so existing rows need an explicit backfill.
UPDATE listings l SET search_vector =
  setweight(to_tsvector('english', coalesce(l.title, '')), 'A') ||
  setweight(to_tsvector('english', coalesce(l.description, '')), 'B') ||
  setweight(to_tsvector('english', coalesce(pt.name, '') || ' ' || coalesce(st.title, '')), 'C')
FROM property_types pt, stay_types st
WHERE pt.id = l.property_type_id AND st.id = l.stay_type_id;

-- Listings with a NULL property_type_id or stay_type_id are skipped by the
-- join above (both must match); catch them separately with LEFT JOINs.
UPDATE listings l SET search_vector =
  setweight(to_tsvector('english', coalesce(l.title, '')), 'A') ||
  setweight(to_tsvector('english', coalesce(l.description, '')), 'B') ||
  setweight(to_tsvector('english', coalesce(pt.name, '') || ' ' || coalesce(st.title, '')), 'C')
FROM listings l2
LEFT JOIN property_types pt ON pt.id = l2.property_type_id
LEFT JOIN stay_types st ON st.id = l2.stay_type_id
WHERE l.listing_id = l2.listing_id AND l.search_vector IS NULL;

CREATE INDEX IF NOT EXISTS idx_listings_search_vector ON listings USING GIN (search_vector);

-- locations: state + district + lower_division_name, mirroring whatever
-- search_text(l) concatenates today (state/district/lower_division_name per
-- the audit), but persisted + indexed instead of computed per-call.
ALTER TABLE locations ADD COLUMN IF NOT EXISTS search_vector tsvector
  GENERATED ALWAYS AS (
    to_tsvector('english',
      coalesce(state, '') || ' ' || coalesce(district, '') || ' ' || coalesce(lower_division_name, '')
    )
  ) STORED;

CREATE INDEX IF NOT EXISTS idx_locations_search_vector ON locations USING GIN (search_vector);

COMMIT;
