// Package sqlparams rewrites atlantis-DSL named parameters
// (`$arg_name`) in a raw SQL body into PostgreSQL positional
// placeholders (`$1, $2, ...`), returning the ordered list of input
// names that drives arg binding at call sites.
//
// The rewrite needs to run in two places: codegen (when emitting typed
// client code that talks to atlantis-server) and runtime (when
// atlantis-server's custom-query dispatcher hands the SQL to pgx).
// Both paths must produce the same string + arg order or the typed
// client and the runtime executor go out of sync. Living in one
// package guarantees that.
//
// Scan rules:
//   - Single-quoted strings, double-quoted identifiers, and
//     pre-existing PG positional placeholders (`$1, $2, ...`) pass
//     through unchanged.
//   - `$<identifier>` where the identifier matches a declared input
//     name is replaced with `$<n>` where n is the input's first-
//     reference position. Repeated references to the same input use
//     the same `$<n>` — one arg per unique input.
//   - `$<identifier>` referencing an undeclared name returns an error.
//     The validator should have caught this earlier; surfacing it
//     again at codegen / runtime is defensive.
//
// The returned `argOrder` slice lists each referenced input name
// exactly once in the order it first appears in the SQL. Callers
// iterate it to bind args in the same order PG expects.
package sqlparams

import (
	"fmt"
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// NormalizeNamed rewrites named parameters in sql into positional ones,
// using inputs to validate that every referenced name is declared.
// Returns the rewritten SQL and the ordered list of referenced input
// names (one entry per unique input, in first-reference order).
func NormalizeNamed(sql string, inputs []dsl.QueryParam) (string, []string, error) {
	ordinal := make(map[string]int, len(inputs))
	for i, p := range inputs {
		ordinal[p.Name] = i + 1
	}
	return normalize(sql, ordinal)
}

// NormalizeNamedRaw is the ordinal-map variant used by procedure-step
// emission where the caller already has the input → 1-based-position
// map from the surrounding procedure scope.
func NormalizeNamedRaw(sql string, ordinal map[string]int) (string, []string, error) {
	return normalize(sql, ordinal)
}

func normalize(sql string, ordinal map[string]int) (string, []string, error) {
	var b strings.Builder
	b.Grow(len(sql))
	var order []string
	localPos := make(map[string]int)

	for i := 0; i < len(sql); i++ {
		c := sql[i]
		switch c {
		case '\'':
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '\'' {
					b.WriteByte('\'')
					if i+1 < len(sql) && sql[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '"':
			b.WriteByte(c)
			i++
			for i < len(sql) {
				if sql[i] == '"' {
					b.WriteByte('"')
					if i+1 < len(sql) && sql[i+1] == '"' {
						b.WriteByte('"')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(sql[i])
				i++
			}
			i--
		case '$':
			if i+1 < len(sql) && sql[i+1] >= '0' && sql[i+1] <= '9' {
				b.WriteByte('$')
				continue
			}
			if i+1 < len(sql) && isLetterOrUnderscore(sql[i+1]) {
				j := i + 1
				for j < len(sql) && isIdentRune(sql[j]) {
					j++
				}
				name := sql[i+1 : j]
				if _, ok := ordinal[name]; !ok {
					return "", nil, fmt.Errorf("$%s is not a declared input parameter", name)
				}
				pos, seen := localPos[name]
				if !seen {
					order = append(order, name)
					pos = len(order)
					localPos[name] = pos
				}
				fmt.Fprintf(&b, "$%d", pos)
				i = j - 1
				continue
			}
			b.WriteByte('$')
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), order, nil
}

func isLetterOrUnderscore(c byte) bool {
	return (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_'
}

func isIdentRune(c byte) bool {
	return isLetterOrUnderscore(c) || (c >= '0' && c <= '9')
}
