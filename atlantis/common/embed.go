// Package common embeds the static common proto definitions (predicates,
// pagination) so tools can materialize them without a checkout of the
// atlantis repo. The embed paths point at the canonical files in this
// directory's v1/ subtree — there is no copy to drift out of sync.
package common

import "embed"

// Protos holds atlantis/common/v1/*.proto. tide generate writes these into
// a caller's output dir as compile-time inputs so a caller's namespace
// protos can `import "atlantis/common/v1/...";`.
//
//go:embed v1/predicates.proto v1/pagination.proto
var Protos embed.FS
