package sim

import "errors"

// ErrNoRows is the simulator's no-rows sentinel. The Pool / Tx adapters
// wrap their internal no-rows condition with this error so runtime.IsNoRows
// (which matches by Error() message) detects it without sim having to
// import the runtime package.
//
// The exact message — "no rows in result set" — is the same string the
// pgx and database/sql drivers emit, so generated code that does an
// errors.Is-equivalent message check sees identical behavior under both
// backends.
var ErrNoRows = errors.New("no rows in result set")

// ErrPKConflict signals an INSERT attempted to use an already-taken
// primary key. Mirrors PG's 23505 unique_violation; the executor wraps
// it with a snippet for diagnostics.
var ErrPKConflict = errors.New("sandbox: primary key conflict")

// ErrColumnNotFound is returned when a parsed statement references a
// column the catalog does not declare. The translator does not check
// columns against the catalog, so missing columns surface at execution.
var ErrColumnNotFound = errors.New("sandbox: unknown column")

// ErrTableNotFound is returned when a statement references a table the
// catalog has no descriptor for.
var ErrTableNotFound = errors.New("sandbox: unknown table")
