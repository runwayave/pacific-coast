# Type mapping

How each supported `.atl` field type maps to PostgreSQL, protobuf, and Go.

## Scope

This page covers the types atlantis currently supports. The following PostgreSQL types are not supported yet: `char(N)`; `time`, `timetz`, and `timestamp` without timezone; `inet`/`cidr`/`macaddr`; `money`; `tsvector`; `point`/`box`/`line`; `oid`; `xml`; `json` (use `jsonb`); `hstore`; `ltree`; enums; domains; composites; ranges. File an issue if you need one.

## Scalars

| `.atl` type | PostgreSQL | Proto | Go |
|---|---|---|---|
| `bigint` | `BIGINT` | `int64` | `int64` |
| `int` | `INTEGER` | `int32` | `int32` |
| `smallint` | `SMALLINT` | `int32` | `int32` |
| `real` | `REAL` | `float` | `float32` |
| `double` | `DOUBLE PRECISION` | `double` | `float64` |
| `boolean` | `BOOLEAN` | `bool` | `bool` |
| `varchar(N)` | `VARCHAR(N)` | `string` | `string` |
| `text` | `TEXT` | `string` | `string` |
| `citext` | `CITEXT` | `string` | `string` |
| `jsonb` | `JSONB` | `bytes` | `[]byte` |
| `bytea` | `BYTEA` | `bytes` | `[]byte` |
| `uuid` | `UUID` | `string` | `string` |
| `numeric(p, s)` | `NUMERIC(p,s)` | `string` | `string` |

Notes:

- `citext` requires the `citext` Postgres extension. The migration emitted by `tide apply` does **not** install it; the operator must `CREATE EXTENSION citext` once per database.
- `numeric` is mapped to `string` on the wire to preserve exact precision. The wire format is the canonical PostgreSQL numeric string (e.g. `"123.4500"`). Unparameterized `numeric` (no `(p, s)`) is not supported.
- `jsonb` payloads are stored as bytes and not parsed by Atlantis. Validity is checked by Postgres at insert time; an invalid JSON body errors at the storage layer, not at the gRPC boundary.
- `uuid` uses the canonical RFC 4122 hyphenated lowercase form on the wire. Non-canonical input is rejected by Postgres at parse time.
- `bigint` over the wire: proto `int64`. If you serialize a response to JSON yourself, proto-JSON encodes `int64` as a string by default to preserve precision.
- `varchar(N)` length is enforced by Postgres (`value too long for type` on `INSERT`); Atlantis does not pre-validate.

## Time

| `.atl` type | PostgreSQL | Proto | Go |
|---|---|---|---|
| `timestamptz` | `TIMESTAMPTZ` | `google.protobuf.Timestamp` | `*timestamppb.Timestamp` |
| `date` | `DATE` | `google.protobuf.Timestamp` | `*timestamppb.Timestamp` |
| `interval` | `INTERVAL` | `google.protobuf.Duration` | `*durationpb.Duration` |

- `date` carries `00:00:00 UTC` as the time-of-day. A caller sending a non-zero time-of-day has the time fraction truncated to midnight on insert.
- `interval` is lossy in conversion: PostgreSQL stores months, days, and microseconds separately, while `google.protobuf.Duration` is a single nanosecond count. Months are normalized as 30 days; days are normalized as 24 hours. Round-trip-stable for sub-month intervals; not stable across months/years.

## Vectors

| `.atl` type | PostgreSQL | Proto | Go |
|---|---|---|---|
| `vector(N)` | `vector(N)` (pgvector) | `repeated float` | `[]float32` |

- Requires the `pgvector` extension. The operator must `CREATE EXTENSION vector` once per database.
- `N` is the dimension. pgvector caps `vector` at 16,000 dimensions; the lexer does not enforce this but Postgres rejects creation past the cap.
- A wire payload whose length doesn't match the declared `N` errors at request time.
- `halfvec`, `sparsevec`, and pgvector's `bit` type are not supported yet.

## Arrays

| `.atl` type | PostgreSQL | Proto | Go |
|---|---|---|---|
| `[]T` | `T[]` | `repeated <T-proto>` | `[]<T-Go>` |

- Only one-dimensional arrays. `[][]T` is rejected at parse time.
- Element type `T` is any scalar above (no `vector`, no nested arrays).
- A null array (nil slice in Go, missing field on the wire) is distinguishable from an empty array (`[]T{}` in Go, present-but-empty on the wire); both round-trip.

## Nullability

Fields are non-nullable by default; declaring `not null` is redundant for `primary` (which implies it). A field declared *without* `not null` is nullable. The proto field gets the `optional` keyword and the Go field becomes a pointer:

| `.atl` type | Go (not null) | Go (nullable) |
|---|---|---|
| `bigint`, `int`, `smallint` | `int64` / `int32` | `*int64` / `*int32` |
| `real`, `double` | `float32` / `float64` | `*float32` / `*float64` |
| `boolean` | `bool` | `*bool` |
| `varchar(N)`, `text`, `citext`, `uuid`, `numeric(p,s)` | `string` | `*string` |
| `timestamptz`, `date` | `*timestamppb.Timestamp` | `*timestamppb.Timestamp` (nil = null) |
| `interval` | `*durationpb.Duration` | `*durationpb.Duration` (nil = null) |
| `jsonb`, `bytea` | `[]byte` | `[]byte` (nil = null) |
| `vector(N)` | `[]float32` | `[]float32` (nil = null) |
| `[]T` | `[]T-Go` | `[]T-Go` (nil = null) |

For `bytes`, `Timestamp`, `Duration`, `vector`, and arrays, the Go type is already nilable; the same value represents both null and (for `bytes`/`vector`/`[]T`) empty. Round-trip preserves the distinction over gRPC because proto3 transmits the field-presence bit separately from the length.

A nullable field with `default <expr>` is treated as nullable by the proto request (proto sees `optional`), but a NULL in the request leaves the column null — the default fires only when the request omits the field entirely.

## Default-bearing fields

Fields declared with `default <expr>` become optional in the proto request even when `not null`:

```
entity Note in app {
  ...
  created_at timestamptz not null default now()
}
```

The proto request has `optional Timestamp created_at`. If the caller leaves it unset, Postgres applies `now()` on insert. If the caller sends a value, that value is used.

## `serial` columns

A field declared `bigint primary serial` (or `int primary serial`) becomes optional in the proto request: the server lets Postgres assign the value from the sequence when the caller omits it. The on-the-wire type is still `int64` / `int32`; only the field-presence semantics differ.

## Related

- [DSL grammar](dsl-grammar.md) — the full type grammar and where types appear inside entity, query, and procedure declarations.
