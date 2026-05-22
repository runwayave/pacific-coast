# atlantis-go

Typed Go SDK for [atlantis](https://github.com/rachitkumar205/atlantis). The module ships protobuf message types and the typed gRPC clients (one per namespace) that caller applications import to talk to an atlantis server. The server lives in the parent directory and is licensed separately.

## Install

```go
import (
    consumerpb "github.com/rachitkumar205/atlantis-go/pb/atlantis/consumer/v1"
    "github.com/rachitkumar205/atlantis-go/client/consumer"
)
```

The module is not yet published to `proxy.golang.org`. Until it is, depend on it via a `replace` directive pointing at a local checkout of the parent monorepo:

```
replace github.com/rachitkumar205/atlantis-go => ../atlantis/clients/go
```

A versioned release is on the roadmap.

## Dependencies

Compile-time: `google.golang.org/grpc` and `google.golang.org/protobuf`. The SDK does not import any atlantis-server packages — atlantis's `internal/` packages are structurally unreachable from any consumer of `atlantis-go` (see the comment in the parent `go.mod`).

Runtime: an endpoint that speaks the atlantis gRPC protocol — typically a deployed `atlantis` server.

## Regeneration

The SDK is regenerated from `.atl` schema files:

1. `.atl` files (in caller repos or `atlantis/testdata/schema/`)
2. `tidectl codegen` → `.proto` files under `atlantis/<namespace>/v1/`
3. `buf generate` → Go pb types in `clients/go/pb/` and typed clients in `clients/go/client/`

`tidectl codegen` runs offline. `buf generate` fetches plugins from the Buf Schema Registry on first run, then uses the local cache; subsequent regenerations don't need network.

Versioned files in `clients/go/`: `go.mod`, `LICENSE`, and this README. The `pb/` and `client/` subtrees are gitignored — produced by `make codegen` against whatever `.atl` files are present.

For cross-caller `references` (a backend entity referencing a vendor entity defined by another caller), the merged schema view comes from `tide pull` against a running atlantis server. Codegen still runs locally against the pulled files.

## License

[Apache License 2.0](LICENSE).

The atlantis server is licensed separately under [BSL 1.1](../../LICENSE). Production use is permitted except offering atlantis on a hosted or embedded basis in competition with the licensor's paid versions. Importing this SDK into a commercial application is unrestricted; only the server deployment is subject to BSL.
