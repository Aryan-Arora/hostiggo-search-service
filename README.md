# hostiggo-search-service

Standalone Go search backend for Hostiggo listings, connecting directly to
the existing Supabase Postgres database (pooled/Supavisor connection, `pgx`)
instead of going through PostgREST.

Ported from an audit of the original Next.js app's search implementation
(two parallel RPCs — `search_listings_by_state`, the live-but-partial path,
and `search_listings`/`listing_search_view`, the more complete but never-
called path). This service targets the **complete spec**
(`search_listings`/`listing_search_view`) as its behavioral baseline —
real rating/room-type/stay-type filtering, PostGIS geo-distance sort, and
date-availability via `NOT EXISTS` — while keeping the **original response
shape and cursor-pagination contract** from the live route, so the frontend
can point at this service unchanged.

## Search implementation

* **Lexical full-text search only.** No `pg_trgm`, no similarity scoring, no
  edit-distance/fuzzy "did you mean" correction anywhere. Free-text queries
  use `websearch_to_tsquery('english', ...)` against a persisted `tsvector`
  column (`listings.search_vector`, `locations.search_vector`), GIN-indexed.
  See [`migrations/0001_add_search_vectors.sql`](migrations/0001_add_search_vectors.sql) —
  neither column/index existed in the original schema; the one existing
  full-text function (`search_locations_partial`) computed `to_tsvector()`
  on the fly per call with no index.
* **`listings.search_vector` covers more than just title/description** — see
  [`migrations/0002_expand_listing_search_vector.sql`](migrations/0002_expand_listing_search_vector.sql).
  Weighted: `A` title, `B` description, `C` property type / stay type /
  district / state / neighborhood, `D` address lines / landmark / amenity
  names. So `q=rohini` matches on address, `q=pool` matches on amenities,
  even when neither word appears in the title or description. Kept in sync
  on writes via triggers on both `listings` (title/description/type/location/
  address changes) and `listing_amenities` (amenity add/remove) — the latter
  exists because amenities live in a separate join table a listings-only
  trigger can't see. One known gap: editing a `locations` row's own
  state/district/neighborhood text does not cascade to listings that
  reference it (documented in the migration; re-run its backfill `UPDATE` or
  call `refresh_listing_search_vector(listing_id)` if that ever happens).
* **New optional `q` filter** on `POST /api/search` — the only contract
  addition to the request shape. Neither original RPC had free-text search;
  both were structured filters (state/district/price/guests/amenities/etc).
* **Default browse order is a diversified price-tier interleave, not
  `listing_id ASC`.** With no `q` and no `latitude`/`longitude` (i.e.
  whenever nothing else is driving relevance/distance ranking), results
  cycle **mid, low, mid, low, high** by price, repeating every 5 positions,
  even with other structured filters (district/price-range/guests/etc.)
  applied. "Low/mid/high" are Postgres `NTILE(3)` tertiles computed fresh
  over whatever that request's own filtered result set contains — not a
  fixed rupee cutoff — so "mid" means the middle third of *these* results,
  consistently, whether that's all 228 listings or a 12-listing district.
  `q` (free-text) or `latitude`/`longitude` (geo-sort) each fully replace
  this with `ts_rank`/distance ordering instead. See `buildQuery` in
  `internal/search/search.go` for the exact interleave math.
* **Pagination**: `cursor` is an opaque row offset in every mode now (it's
  no longer `listing_id`-based even for the plain/default case, since the
  price-tier interleave isn't `listing_id`-monotonic either). The client
  never needs to know which ordering mode is active — it just echoes back
  whatever `cursor` value the previous response returned.
* **`totalCount`** is computed with the *same* `WHERE`-clause builder as the
  page query (see `whereBuilder` in `internal/search/search.go`), closing
  the drift risk the original migration's own comments flagged between
  `search_listings_by_state` and its separate `_count` RPC.

## Access control (replacing RLS)

This service uses a direct Postgres connection, which bypasses Supabase
Row Level Security entirely — there is no anon JWT to scope by. The
original RLS policies (see the audit) are reimplemented as explicit checks:

* `listings`, `locations`, `listing_media`, `listing_amenities`, `amenities`,
  `property_types`, `stay_types`, `listing_calendar` were all openly
  readable (`qual: true`) in the original policies — this service queries
  them the same way, with `WHERE l.is_active = TRUE` enforced in code as the
  one real predicate those policies encoded.
* `bookings` originally restricted `SELECT` to the owning guest/host. The
  live app could only read it for availability-blocking via a service-role
  key that bypasses RLS. This service's date-availability check
  (`internal/search/search.go`, `buildFilters`) reads **only**
  `listing_id`/`start_date`/`end_date`/`status_id` from `bookings` — never
  guest/host-identifying columns — as the explicit, minimal, public-safe
  replacement for that bypass. Run the connection as a Postgres role granted
  `SELECT` on exactly the columns/tables this service needs (see below),
  not the Supabase service-role/superuser.

**Recommended DB role** (adjust to your actual grants):
```sql
CREATE ROLE search_service_ro NOLOGIN;
GRANT USAGE ON SCHEMA hostiggo_testing_schema TO search_service_ro;
GRANT SELECT ON hostiggo_testing_schema.listings, hostiggo_testing_schema.locations,
  hostiggo_testing_schema.listing_media, hostiggo_testing_schema.listing_amenities,
  hostiggo_testing_schema.amenities, hostiggo_testing_schema.property_types,
  hostiggo_testing_schema.stay_types, hostiggo_testing_schema.review,
  hostiggo_testing_schema.listing_calendar TO search_service_ro;
GRANT SELECT (listing_id, start_date, end_date, status_id)
  ON hostiggo_testing_schema.bookings TO search_service_ro;
```
Use a login role that inherits `search_service_ro` for the pooled connection
string in `DATABASE_URL`.

## API

### `POST /api/search`
Same request/response shape as the original route, plus optional `filters.q`.
See `internal/search/types.go` for the exact JSON contract.

### `GET /api/locations`
`q` (full-text autocomplete), `limit` (default 22, clamped 1–100), `popular=1`
(ranked by active listing count). Same response shape as the original route.

### `GET /api/hotels`
Homepage teaser: `locationId` (required — returns `{data: []}` if missing or
non-numeric, never an error), `limit` (default 4, clamped 1–100). Returns up
to `limit` active listings for that location, shaped like the `listing`
object in `POST /api/search`'s results. Deliberately fails soft (empty array,
`200`, not a `4xx/5xx`) on bad input or a query error — it backs a homepage
widget that shouldn't be able to break the page around it.

## CORS

Enabled by default (`CORS_ALLOW_ORIGIN=*`) — this API has no auth header or
cookies to protect either way (same as the original anon-key Supabase path),
so a permissive default doesn't widen access, it just lets a browser call it
cross-origin. Set `CORS_ALLOW_ORIGIN` to the real frontend's origin once
known, or to an empty string to disable CORS headers for a same-origin-only
deployment.

## Known gaps vs. a full port

* `ST_AsGeoJSON(boundary)` / state-bounds-for-map lookup isn't wired up yet;
  `stateBounds` is always omitted. Add a `locations` boundary query if the
  frontend needs it.
* This is a pre-launch dataset (228 listings). No rate limiting or query
  timeout enforcement beyond `QUERY_TIMEOUT_MS` (not yet wired into
  per-request contexts) — add before assuming production scale.

## Running locally

```bash
cp .env.example .env   # fill in DATABASE_URL
go run ./cmd/server
```

Apply the migration against your Supabase project (via the SQL editor, or
`psql "$DATABASE_URL" -f migrations/0001_add_search_vectors.sql`) before
starting the service — `search_vector` columns must exist first.

## Deploying

A `Dockerfile` and `fly.toml` are included but nothing is deployed by this
scaffold.

```bash
fly launch --no-deploy   # adopts fly.toml
fly secrets set DATABASE_URL='...'
fly deploy
```

Railway: create a project from this repo, set `DATABASE_URL` (and optionally
`DB_SCHEMA`/`PORT`) as environment variables — the `Dockerfile` is picked up
automatically.
