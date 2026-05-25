package entity

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// entitySnapshot is an immutable point-in-time view of all entity and
// custom-query metadata derived from one IR. Swapped atomically on
// hot-reload via Server.snapshot; handlers load the current snapshot at
// each request. The old snapshot stays alive until all in-flight
// requests holding a reference complete.
type entitySnapshot struct {
	entities    map[string]*entityMeta
	customMeta  map[string]*customQueryMeta
	contentHash string
}

// buildSnapshot constructs a complete snapshot from an IR. Pure
// function — no side effects, no gRPC registration.
func buildSnapshot(ir *dsl.IR, contentHash string) (*entitySnapshot, error) {
	snap := &entitySnapshot{
		entities:    make(map[string]*entityMeta, len(ir.Entities)),
		customMeta:  make(map[string]*customQueryMeta, len(ir.Queries)),
		contentHash: contentHash,
	}

	for i := range ir.Entities {
		e := &ir.Entities[i]
		meta := buildEntityMeta(e, ir)

		fd, err := buildProtoDescriptors(e)
		if err != nil {
			return nil, fmt.Errorf("entity %s: %w", e.ID(), err)
		}
		resolveProtoDescriptors(meta, fd)

		if meta.msgDesc == nil {
			return nil, fmt.Errorf("entity %s: entity message descriptor not built", e.ID())
		}
		if meta.getRequestDesc == nil {
			return nil, fmt.Errorf("entity %s: GetRequest descriptor not built", e.ID())
		}

		snap.entities[e.ID()] = meta
	}

	for i := range ir.Queries {
		cq := &ir.Queries[i]
		parts := splitEntityID(cq.Owner)
		ns := parts[0]

		cqm := &customQueryMeta{
			query:     cq,
			sql:       cq.SQL,
			inputCols: cq.Inputs,
			timeoutMS: 2000,
		}

		if cq.Output.AsEntityID != "" {
			cqm.asEntity = true
			if em, ok := snap.entities[cq.Output.AsEntityID]; ok {
				cqm.entityMeta = em
			}
		} else {
			cqm.outputCols = cq.Output.Columns
		}

		fd, err := buildCustomQueryDescs(cq, ns)
		if err != nil {
			return nil, fmt.Errorf("custom query %s: %w", cq.Name, err)
		}

		cqm.requestDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Request"))
		cqm.responseDesc = fd.Messages().ByName(protoreflect.Name(cq.Name + "Response"))

		if len(cq.Output.Columns) > 0 && cqm.responseDesc != nil {
			rowName := protoreflect.Name(cq.Name + "Response_Row")
			cqm.rowDesc = cqm.responseDesc.Messages().ByName(rowName)
		}

		key := ns + ":" + cq.Name
		snap.customMeta[key] = cqm
	}

	return snap, nil
}
