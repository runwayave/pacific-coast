package codegen

import (
	"strings"

	"github.com/rachitkumar205/atlantis/internal/dsl"
)

// FilterIR returns a copy of ir keeping only declarations whose namespace
// is in namespaces. Used for caller-local SDK generation: a caller
// generates types only for the namespaces it consumes, not the whole
// merged schema.
//
// Cross-namespace concerns take care of themselves. FK fields that point
// at a dropped namespace stay as scalar columns (a FK is just a typed
// column, never a proto import). Inbound-include enum variants are
// recomputed by EmitProto via computeInboundRefs over the filtered entity
// set, so a variant whose source entity was dropped is never emitted —
// no dangling reference, no wire change for the kept entities.
func FilterIR(ir *dsl.IR, namespaces []string) *dsl.IR {
	if ir == nil {
		return nil
	}
	keep := make(map[string]bool, len(namespaces))
	for _, ns := range namespaces {
		keep[ns] = true
	}

	out := &dsl.IR{Version: ir.Version}

	for _, e := range ir.Entities {
		if keep[e.Namespace] {
			out.Entities = append(out.Entities, e)
		}
	}
	for _, q := range ir.Queries {
		if keep[ownerNamespace(q.Owner)] {
			out.Queries = append(out.Queries, q)
		}
	}
	for _, p := range ir.Procedures {
		if keep[ownerNamespace(p.Owner)] {
			out.Procedures = append(out.Procedures, p)
		}
	}
	for _, j := range ir.Jobs {
		if keep[j.Namespace] {
			out.Jobs = append(out.Jobs, j)
		}
	}
	for _, w := range ir.Workflows {
		if keep[w.Namespace] {
			out.Workflows = append(out.Workflows, w)
		}
	}
	for _, ep := range ir.Ephemerals {
		if keep[ep.Namespace] {
			out.Ephemerals = append(out.Ephemerals, ep)
		}
	}

	return out
}

// ownerNamespace extracts the namespace from a canonical "namespace.Entity"
// owner id. Custom queries/procedures carry their namespace this way rather
// than a dedicated field.
func ownerNamespace(owner string) string {
	if i := strings.IndexByte(owner, '.'); i >= 0 {
		return owner[:i]
	}
	return owner
}
