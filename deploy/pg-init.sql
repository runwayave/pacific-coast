-- Runs once on first DB initialization (docker-entrypoint-initdb.d).
-- These are the three extensions atlantis schemas can require today:
--   vector       — when a `.atl` declares vector(N) columns
--   timescaledb  — when a `.atl` declares a hypertable
--   citext       — when a `.atl` declares a citext column
-- Pre-enabling all three lets `tide apply` succeed against this bundle
-- without the server having to CREATE EXTENSION at apply time. To add
-- a new extension: append a CREATE EXTENSION
-- line, then either rebuild the postgres image bundle or wipe the
-- atl-pg volume so init re-runs.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS citext;
