-- Data fix, not an alias: location_id 66 has state = 'Haryāna' (diacritic
-- typo — U+0101 LATIN SMALL LETTER A WITH MACRON) instead of 'Haryana'.
-- Unlike Delhi/New Delhi or Bangalore/Bengaluru, this isn't two real names
-- in common use; it's a single malformed value. Confirmed before applying:
--   - No existing row already has (state='Haryana', district='Faridabad',
--     lower_division_name='Main Bazaar Road', lower_division_type='locality',
--     pincode='121001') — so this UPDATE can't collide with the table's
--     unique constraint on that (state,district,lower_division_name,
--     lower_division_type,pincode) tuple.
--   - Zero listings reference location_id 66 today, so this has no effect
--     on current search results; it's a correctness fix for that row (and
--     any future listing assigned to it) going forward.

BEGIN;

UPDATE locations SET state = 'Haryana' WHERE state = 'Haryāna';

COMMIT;
