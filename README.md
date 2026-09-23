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
* **New optional `q` filter** on `POST /api/search` — the only contract
  addition. Neither original RPC had free-text search; both were structured
  filters (state/district/price/guests/amenities/etc). When `q` is present,
  results are ordered by `ts_rank` instead of `listing_id`.
* **Pagination**: `cursor` stays wire-compatible as an opaque number.
  - No `q` and no `latitude`/`longitude`: cursor is `listing_id` (identical
    semantics to the original `search_listings_by_state`).
  - With `q` or geo sort: ordering is by relevance/distance, which isn't
    monotonic with `listing_id`, so cursor is a row offset instead. The
    client never needs to know which mode is active — it just echoes back
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

## Known gaps vs. a full port

* `GET /api/hotels` (homepage teaser) was out of scope — it's not a search
  endpoint per the audit, and wasn't required by the brief.
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
