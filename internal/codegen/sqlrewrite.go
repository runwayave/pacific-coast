package codegen

import (
	"regexp"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// atlantisTableRe matches `atlantis.<ident>` — atlantis's flat-name
// convention for entity tables (`<namespace>_<snake_entity>` under the
// `atlantis` schema). User-authored SQL inside `procedure` and `query`
// touches blocks references tables by this form because it's where
// atlantis stored them historically. When an entity declares `table
// "..."` to point at a pre-existing prod table, the auto-emitted CRUD
// follows the override but the user's hand-written SQL still names the
// atlantis-managed location — so we rewrite the references at emit
// time. Entities without an override map back to the same name they
// started with (modulo proper quoting); the function is a no-op for
// schemas that use atlantis's default layout.
//
// Matched against the raw SQL rather than its parsed AST because in
// practice users never put table names inside string literals; if a
// real case emerges we'd switch to a pg_query_go walk.
var atlantisTableRe = regexp.MustCompile(`\batlantis\.([a-zA-Z_][a-zA-Z0-9_]*)\b`)

// rewriteAtlantisTableRefs substitutes atlantis-flat-name table
// references in user-authored SQL with each entity's resolved physical
// table name. Called by the custom-query and procedure-raw-step
// emitters before the SQL is embedded into generated Go handlers.
//
// References that don't resolve to an entity in the IR pass through
// unchanged — that's a validation concern the validator already
// caught (rejects unknown atlantis.X references at plan time).
func rewriteAtlantisTableRefs(sql string, ir *dsl.IR) string {
	if ir == nil {
		return sql
	}
	flat := make(map[string]*dsl.Entity, len(ir.Entities))
	for i := range ir.Entities {
		e := &ir.Entities[i]
		flat[tableName(e)] = e
	}
	return atlantisTableRe.ReplaceAllStringFunc(sql, func(match string) string {
		name := match[len("atlantis."):]
		if e, ok := flat[name]; ok {
			return qualifiedTable(e)
		}
		return match
	})
}
