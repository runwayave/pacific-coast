# DSL reference

Syntax reference for `.atl` files.

## Notation

In the grammar productions below:

- `[X]` is optional.
- `{X}` is zero or more.
- `X | Y` is alternation.
- Quoted text appears literally; `Ident`, `Type`, etc. are productions.

Whitespace separates tokens but is otherwise insignificant. Line comments start with `//` and run to end of line. There are no block comments.

## Top-level

```
File         = { Declaration }
Declaration  = Entity | Hypertable | Query | Procedure
```

## Entities

```
Entity = "entity" Ident "in" Ident "{" EntityBody "}"

EntityBody =
    { FieldDecl }
    [ "composite_pk" "by" IdentList ]
    { "unique" "by" IdentList }
    { "index" "by" IndexFieldList }
    { [ "unique" ] "index" "partial" "by" IdentList "where" PartialPredicate }
    { "index" "hnsw" "on" Ident "ops" VectorOps }
    { "index" "gin" "on" Ident }
    [ "soft_delete" "by" Ident ]
    [ "touch_on_update" "by" Ident ]
    [ "partition" "by" Ident ]
    [ "table" StringLiteral ]
    [ CacheBlock ]

IdentList      = Ident { "," Ident }

IndexFieldList = IndexField { "," IndexField }
IndexField     = ( Ident | "expr" StringLiteral ) [ "asc" | "desc" ]

VectorOps      = "cosine" | "l2" | "ip"

PartialPredicate = OrExpr
OrExpr   = AndExpr { "or"  AndExpr }
AndExpr  = NotExpr { "and" NotExpr }
NotExpr  = "not" NotExpr | Primary
Primary  =
    "(" PartialPredicate ")"
  | Operand PartialOp Operand
  | Operand "is" [ "not" ] "null"
  | Operand [ "not" ] "in" "(" Operand { "," Operand } ")"
  | Operand                              // bare boolean column

Operand  = Base { "::" Type }
Base     = StringLiteral | NumericLiteral | BooleanLiteral
         | Ident                                       // column
         | Ident "(" [ Operand { "," Operand } ] ")"   // immutable function call
         | Case
Case     = "case" { "when" PartialPredicate "then" Operand } [ "else" Operand ] "end"
Type     = Ident [ "(" Integer { "," Integer } ")" ]

PartialOp = "=" | "!=" | "<" | "<=" | ">" | ">="
```

`and` binds tighter than `or`; parenthesise to override. `and`/`or` (and the CASE
keywords `case`/`when`/`then`/`else`/`end`) are not reserved words — a field may
still be named `and`, `case`, etc. everywhere except where it would start one of
those constructs. `NumericLiteral` covers both integers and floats (`3`, `3.14`).
Operands may be columns, literals, immutable function calls (`lower(email)`),
casts (`amount::numeric`), and CASE expressions.

Entity names use `PascalIdent`; namespaces use `SnakeIdent`. Underscores are syntactically valid in namespaces but not conventional.

The namespace becomes the package segment under `output_dir/` and the schema prefix on the generated table name: `<namespace>_<snake_entity>`.

### Fields

```
FieldDecl = Ident Type { Modifier }

Modifier =
    "primary"
  | "serial"
  | "not" "null"
  | "default" DefaultExpr
  | "unique"
  | "references" QualifiedField [ "on" "delete" RefAction ]
  | "check" "\"" SQLExpr "\""

DefaultExpr =
    FunctionCall      // e.g. now(), gen_random_uuid()
  | NumericLiteral    // e.g. 0, 3.14
  | StringLiteral     // single-quoted, e.g. 'pending'
  | BooleanLiteral    // true, false

QualifiedField = [ Namespace "." ] Entity "." Field

RefAction = "cascade" | "set" "null" | "restrict" | "no" "action"
```

Field names use `SnakeIdent`. Modifier order is flexible, but the lexer rejects incompatible combinations: `serial` with `default`, two `primary` modifiers on different fields, etc.

`QualifiedField`: same-namespace references can omit the namespace (`Customer.id`); cross-namespace references qualify (`vendor.Product.id`). The referenced field must be declared `primary` or have a column-level `unique`.

`check`: the string body is parsed as a Postgres boolean expression. Anything valid inside `CREATE TABLE ... CHECK (...)` is accepted.

### Field types

| Type | PostgreSQL | Notes |
|---|---|---|
| `bigint` | `BIGINT` | |
| `int` | `INTEGER` | |
| `smallint` | `SMALLINT` | |
| `real` | `REAL` | 32-bit float |
| `double` | `DOUBLE PRECISION` | 64-bit float |
| `boolean` | `BOOLEAN` | |
| `varchar(N)` | `VARCHAR(N)` | |
| `text` | `TEXT` | |
| `citext` | `CITEXT` | case-insensitive text |
| `jsonb` | `JSONB` | |
| `bytea` | `BYTEA` | binary |
| `timestamptz` | `TIMESTAMPTZ` | timestamp with timezone |
| `date` | `DATE` | |
| `interval` | `INTERVAL` | |
| `numeric(p, s)` | `NUMERIC(p,s)` | arbitrary precision |
| `uuid` | `UUID` | |
| `vector(N)` | `vector(N)` | pgvector extension; index with `index hnsw on <field> ops <...>` |
| `[]T` | `T[]` | array; element type `T` is any scalar above except `vector` and `[]T` |

Go and proto mappings are in [the type mapping reference](dsl-types.md).

### Modifier semantics

- `primary` — primary key. Exactly one field, unless `composite_pk by` is used at the entity level. The two are mutually exclusive.
- `serial` — Postgres assigns the value via a sequence. Valid only with `bigint primary` or `int primary`. Incompatible with `default`.
- `not null` — disallows null. Implied by `primary`.
- `default <expr>` — Postgres default expression. See `DefaultExpr` above.
- `unique` — Postgres column-level `UNIQUE`. For multi-column, use `unique by` at the entity level.
- `references <Entity>.<field>` — foreign key. `on delete` accepts `cascade`, `set null`, `restrict`, `no action`. `on update` is not supported; see [Known gaps](#known-gaps).
- `check "<predicate>"` — Postgres `CHECK` constraint.

### Entity-level clauses

- `composite_pk by f1, f2` — composite primary key. Member fields must each be `not null`. Mutually exclusive with per-field `primary`.
- `unique by f1, f2` — multi-column unique constraint. May appear multiple times. For a single column, use the per-field `unique` modifier instead.
- `index by f1, f2` — non-unique B-tree index. May appear multiple times. Each field may instead be an expression (`expr "lower(email)"`) and may carry a per-field `asc` or `desc` (e.g. `index by created_at desc`).
- `index partial by f1, f2 where <predicate>` — partial index. The predicate is a boolean expression over the entity's columns and constants: `and` / `or` / `not` / parentheses combining comparisons (`=`, `!=`, `<`, `<=`, `>`, `>=`; inequality is written `!=`), `field is [not] null`, `field [not] in (...)`, and bare boolean columns (`where is_default`). Operands may be columns, literals, immutable function calls (`lower(email)`), casts (`amount::numeric`), and `case … end` expressions. It must be a *legal Postgres index predicate* — no subqueries or aggregates; a volatile function (`now()`, `random()`) is rejected by Postgres at apply time.
- `unique index partial by f1, f2 where <predicate>` — partial **unique** index (`CREATE UNIQUE INDEX … WHERE …`). Use it for uniqueness scoped by a predicate — e.g. `unique index partial by sku where deleted_at is null` makes `sku` unique among non-soft-deleted rows, or `unique index partial by user_id where is_default` for one default per user. A Postgres UNIQUE *constraint* can't be partial, so `unique` / `unique by` can't express this. Same predicate grammar as `index partial`.
- `index hnsw on <field> ops <cosine|l2|ip>` — pgvector HNSW index over a `vector(N)` field. `ops` picks the operator class: `cosine`, `l2` (Euclidean), or `ip` (inner product).
- `index gin on <field>` — GIN index, for `jsonb` and array fields.

The only unique-index form is `unique index partial`; `index by`, `index hnsw`, `index gin`, and the non-`unique` `index partial` are all non-unique. Non-partial uniqueness is declared with the per-field `unique` modifier or entity-level `unique by` (which emit UNIQUE constraints). A live `CREATE UNIQUE INDEX` the schema doesn't account for is treated as drift — `tide apply` refuses it unless `ATLANTIS_ALLOW_INDEX_DRIFT=1`; a declared `unique index partial` whose predicate matches the live one is recognized and not drift. See [`tide apply`](cli-tide.md).
- `soft_delete by <field>` — replaces row deletion with setting `<field>` (must be `timestamptz`) to `now()`. Reads filter `<field> IS NULL` automatically.
- `touch_on_update by <field>` — Postgres trigger sets `<field>` (must be `timestamptz`) to `now()` on every `UPDATE`.
- `partition by <field>` — Atlantis-level multi-tenant partition. Not Postgres table partitioning. Generated read RPCs inject `<field> = <caller-partition>` into the predicate; callers cannot override. The caller partition is read from the auth context.
- `table "<schema.table>"` — overrides the physical table name. Without it, atlantis stores the entity at `atlantis.<namespace>_<snake_entity>`. The value's shape is `[schema.]table`, each segment matching `[A-Za-z_][A-Za-z0-9_]*`; a bare name (`table "vendors"`) lands in `public`. Foreign keys whose target carries the modifier render `REFERENCES "<schema>"."<table>"`. Changing the value on a previously-applied entity is classified `cross_caller_breaking` and rejected by `tide plan`; atlantis does not auto-rename. Used when adopting an existing database — see [Adopt an existing database](../guides/adopt-an-existing-database.md).

### Cache block

```
CacheBlock = "cache" "{" "read_through" "ttl" "=" Duration [ "tag" "=" StringLiteral ] "}"

Duration = Integer DurationUnit
DurationUnit = "ns" | "us" | "ms" | "s" | "m" | "h"
```

`read_through` is currently the only supported caching mode.

The `tag` is a double-quoted string with `{field_name}` interpolation placeholders. Field names inside `{...}` must exist on the entity. Cache entries with the same resolved tag are invalidated as a group. See [Caching and invalidation](../concepts/caching-and-invalidation.md).

## Queries

```
Query = "query" Ident "for" Ident "{" QueryBody "}"

QueryBody =
    "input"  "{" ParamList "}"
    OutputDecl
    SqlBlock

OutputDecl = "output" "as" Ident
           | "output" "{" ParamList "}"

ParamList = Ident ":" Type { "," Ident ":" Type }

SqlBlock = "sql" "touches" "(" IdentList ")" "{" SQL "}"
```

The `for <Ident>` after the query name names the entity the query semantically belongs to. It becomes a method on that entity's generated client and must appear in `touches(...)`.

- `output as <Entity>` returns rows of that entity. The SQL must project every column the entity declares.
- `output { ... }` returns an ad-hoc row type. The SQL must project columns matching the declared names and types.

Parameters in the SQL body use `$name` syntax. Atlantis rewrites them to Postgres positional placeholders (`$1`, `$2`, ...) before execution. The body is validated when you run `tide apply`.

`touches(...)` lists the entities the query reads. The cache layer uses it for query-result invalidation.

## Procedures

```
Procedure = "procedure" Ident "for" Ident "{" ProcedureBody "}"

ProcedureBody =
    "input" "{" ParamList "}"
    "steps" "{" SqlBlock { SqlBlock } "}"
```

The `for <Ident>` after the procedure name names the entity the procedure belongs to (same semantics as a query). It becomes a method on that entity's generated client.

Steps run inside one Postgres transaction. The transaction commits when every step succeeds; any error rolls back the entire transaction. Each step's `touches(...)` declares the write set the cache outbox invalidates after commit.

Procedures do not return rows. Read the result with a separate query.

## Hypertables

```
Hypertable = "hypertable" Ident "in" Ident "{" HypertableBody "}"

HypertableBody =
    { FieldDecl }
    "partition_field" Ident
    "chunk_time_interval" Duration
    [ other EntityBody clauses... ]
```

`partition_field` is the TimescaleDB hypertable partition column and must be a `timestamptz` field declared in the body. The entity-level `partition by` clause is a different mechanism (Atlantis multi-tenant partition); both may coexist on one hypertable.

`chunk_time_interval` uses the same `Duration` syntax as cache TTLs.

Hypertables accept every entity-body clause (indexes, unique constraints, soft delete, cache block, the multi-tenant `partition by`).

## Identifiers

```
PascalIdent = [A-Z][A-Za-z0-9]*
SnakeIdent  = [a-z][a-z0-9_]*
```

Entity, namespace, query, and procedure names use `PascalIdent`. Field, input, and output names use `SnakeIdent`.

## Reserved words

The following are reserved everywhere and cannot be used as identifiers:

```
entity, hypertable, query, procedure,
in, for, input, output, steps, sql, touches, as,
primary, serial, not, null, default, unique, references, check,
on, update, delete, cascade, set, restrict, no, action,
composite_pk, index, partial, where, is,
hnsw, ops, cosine, l2, ip, gin, asc, desc, expr,
soft_delete, touch_on_update, partition, by,
table,
cache
```

The following are contextual — they are keywords only inside specific blocks and may otherwise be used as identifiers:

```
read_through, ttl, tag         // only inside cache { ... }
partition_field, chunk_time_interval   // only inside hypertable { ... }
```

## Known gaps

This reference does not yet cover: the `ivfflat` vector-index method (only `hnsw` is supported), GiST indexes, `on update` foreign-key actions, enum types, view declarations, and import statements. Tracked in the project issue tracker.
