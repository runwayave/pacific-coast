package entity

import (
	"fmt"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/rachitkumar205/atlantis/internal/cache/queryresult"
	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/runtime"
)

// Server is the dynamic entity server that reads the DSL IR at startup
// and serves every entity's CRUD RPCs without compiled proto stubs.
// Handlers load metadata from the current snapshot at each request,
// enabling hot-reload via atomic pointer swap.
type Server struct {
	pool       runtime.Pool
	cache      runtime.Cache
	outbox     runtime.Outbox
	queryCache *queryresult.Cache
	snapshot   atomic.Pointer[entitySnapshot]
}

// NewServer constructs a dynamic entity server.
func NewServer(pool runtime.Pool, cache runtime.Cache, outbox runtime.Outbox, qc *queryresult.Cache) *Server {
	s := &Server{
		pool:       pool,
		cache:      cache,
		outbox:     outbox,
		queryCache: qc,
	}
	s.snapshot.Store(&entitySnapshot{
		entities:   make(map[string]*entityMeta),
		customMeta: make(map[string]*customQueryMeta),
	})
	return s
}

// Register reads the IR, builds the initial entity snapshot, and
// registers one gRPC service per entity plus per-namespace
// CustomService descriptors for custom queries.
func (s *Server) Register(grpcSrv *grpc.Server, ir *dsl.IR) error {
	if ir == nil {
		return fmt.Errorf("entity.Register: nil IR")
	}

	snap, err := buildSnapshot(ir, "")
	if err != nil {
		return err
	}
	s.snapshot.Store(snap)

	for _, meta := range snap.entities {
		desc := buildGRPCServiceDesc(s, meta)
		grpcSrv.RegisterService(&desc, nil)
	}

	if len(ir.Queries) > 0 {
		s.registerCustomServices(grpcSrv, snap)
	}

	return nil
}

// Reload builds a new snapshot from the IR and swaps it atomically.
// In-flight requests on the old snapshot complete unaffected.
func (s *Server) Reload(ir *dsl.IR, contentHash string) error {
	snap, err := buildSnapshot(ir, contentHash)
	if err != nil {
		return fmt.Errorf("entity.Reload: %w", err)
	}
	s.snapshot.Store(snap)
	return nil
}

// ContentHash returns the content hash of the currently loaded snapshot.
func (s *Server) ContentHash() string {
	snap := s.snapshot.Load()
	if snap == nil {
		return ""
	}
	return snap.contentHash
}

// buildGRPCServiceDesc constructs the grpc.ServiceDesc for one entity.
// Handlers capture the entity ID and look up metadata from the current
// snapshot at request time, enabling hot-reload.
func buildGRPCServiceDesc(s *Server, meta *entityMeta) grpc.ServiceDesc {
	ns := goNamespace(meta.entity.Namespace)
	entityID := meta.entityID
	name := meta.entity.Name
	serviceName := fmt.Sprintf("atlantis.%s.v1.%sService", ns, name)

	methods := []grpc.MethodDesc{
		{MethodName: "Get" + name, Handler: makeHandler(s, entityID, "Get", ns, name)},
		{MethodName: "Create" + name, Handler: makeHandler(s, entityID, "Create", ns, name)},
		{MethodName: "Update" + name, Handler: makeHandler(s, entityID, "Update", ns, name)},
		{MethodName: "Delete" + name, Handler: makeHandler(s, entityID, "Delete", ns, name)},
		{MethodName: "BatchGet" + name, Handler: makeHandler(s, entityID, "BatchGet", ns, name)},
		{MethodName: "Query" + name, Handler: makeHandler(s, entityID, "Query", ns, name)},
	}

	return grpc.ServiceDesc{
		ServiceName: serviceName,
		HandlerType: nil,
		Methods:     methods,
		Streams:     []grpc.StreamDesc{},
		Metadata:    fmt.Sprintf("atlantis/%s/v1/%s.proto", ns, name),
	}
}

// registerCustomServices registers one gRPC CustomService per namespace
// from the pre-built snapshot. Handlers capture the query key and look
// up metadata from the current snapshot at request time.
func (s *Server) registerCustomServices(grpcSrv *grpc.Server, snap *entitySnapshot) {
	type nsGroup struct {
		ns      string
		methods []grpc.MethodDesc
	}
	groups := make(map[string]*nsGroup)

	for key, cqm := range snap.customMeta {
		parts := splitEntityID(cqm.query.Owner)
		ns := parts[0]

		g, ok := groups[ns]
		if !ok {
			g = &nsGroup{ns: ns}
			groups[ns] = g
		}
		g.methods = append(g.methods, grpc.MethodDesc{
			MethodName: cqm.query.Name,
			Handler:    makeCustomHandler(s, key, ns),
		})
	}

	for _, g := range groups {
		goNS := goNamespace(g.ns)
		desc := grpc.ServiceDesc{
			ServiceName: fmt.Sprintf("atlantis.%s.v1.CustomService", goNS),
			HandlerType: nil,
			Methods:     g.methods,
			Streams:     []grpc.StreamDesc{},
			Metadata:    fmt.Sprintf("atlantis/%s/v1/custom.proto", goNS),
		}
		grpcSrv.RegisterService(&desc, nil)
	}
}
