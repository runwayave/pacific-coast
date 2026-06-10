-- Caller identity aliases. Lets a registered caller's cert CN satisfy
-- visible_to predicates declared for OTHER identity names — the
-- PostgreSQL-roles / AD-SID / DNS-CNAME pattern of decoupling schema
-- declarations from operational identity.
--
-- Why:
--   - A cert CN rename (e.g. `vendor` → `backstage`) shouldn't require
--     rewriting every `.atl` file that has `visible_to "vendor"`.
--   - Multi-environment deploys may want the same `.atl` schema to
--     resolve to different physical identities per environment.
--   - Cutover windows benefit from "both names work" semantics so the
--     rename can ship gradually instead of as a coordinated big-bang.
--
-- Semantics:
--   - aliases is an unordered set of strings. Empty {} = no aliases
--     (today's behavior, fully back-compat).
--   - At authz time, a visible_to predicate matches when:
--       a) visible_to is empty or "*"  (permissive), OR
--       b) visible_to == caller.cn,    (canonical), OR
--       c) visible_to is in caller.aliases.
--   - Aliases NEVER expand authentication: a cert authenticates as its
--     CN. Aliases only widen what visible_to predicates that CN matches.
--   - Aliases are operator-controlled (admin RPC + console sudo gate).
--     A caller cannot self-declare aliases.
--
-- A GIN index on aliases makes "is X in any caller's aliases" lookups
-- fast — useful for future operator tooling like "who can handle
-- visible_to=vendor?" and for the reverse-lookup at authz time when we
-- want to check membership without scanning the table.

ALTER TABLE atlantis.caller_identities
    ADD COLUMN IF NOT EXISTS aliases TEXT[] NOT NULL DEFAULT '{}';

-- GIN index supports the @> and = ANY() operators we'll use for fast
-- membership tests. Stored as a separate object (not partial) so
-- adding/removing aliases doesn't churn the index size.
CREATE INDEX IF NOT EXISTS caller_identities_aliases_idx
    ON atlantis.caller_identities USING GIN (aliases);
