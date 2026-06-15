package dsl

import "fmt"

// TokenKind enumerates every kind of token produced by the lexer.
// Order matters only for debugging — values are not part of any wire contract.
type TokenKind int

const (
	TokError TokenKind = iota
	TokEOF

	// Literals and identifiers
	TokIdent
	TokInt
	TokFloat // 3.14 — a digit run, a dot, then at least one more digit
	TokString
	TokDuration // 30s, 10m, 1h, 7d

	// Punctuation
	TokLBrace   // {
	TokRBrace   // }
	TokLParen   // (
	TokRParen   // )
	TokLBracket // [
	TokRBracket // ]
	TokComma    // ,
	TokColon    // :
	TokDColon   // :: (cast operator)
	TokEquals   // =
	TokNotEq    // !=
	TokLT       // <
	TokLE       // <=
	TokGT       // >
	TokGE       // >=
	TokDot      // .

	// Keywords (one constant per reserved word — keeps parser exhaustive switches honest)
	TokEntity
	TokHypertable
	TokIn
	TokPrimary
	TokIdentity
	TokSerial
	TokNot
	TokNull
	TokUnique
	TokDefault
	TokCheck
	TokBackfill
	TokReferences
	TokHasMany
	TokHasOne
	TokVia
	TokIndex
	TokBy
	TokOn
	TokHnsw
	TokOps
	TokCosine
	TokL2
	TokIp
	TokGin
	TokPartial
	TokWhere
	TokIs
	TokAsc
	TokDesc
	TokCache
	TokReadThrough
	TokTtl
	TokTag
	TokInvalidateOn
	TokWrite
	TokSelf
	TokConsistency
	TokStrict
	TokEventual
	TokQueryTimeout
	TokSoftDelete
	TokTouchOnUpdate
	TokPartition
	TokTable
	TokExpr
	TokAs
	TokDeferrable
	TokTrue
	TokFalse
	TokNow
	TokRaw
	TokDelete
	TokUpdate
	TokCascade
	TokRestrict
	TokSet

	// DSL constructs for caller-defined custom queries and multi-step
	// procedures. These are the escape hatch for workloads
	// QueryX can't model (GROUP BY aggregations, DISTINCT ON, seeded-
	// random sampling, multi-entity transactions). Declared in caller
	// `.atl` files so atlantis retains static visibility into every
	// query in the platform.
	TokQuery
	TokProcedure
	TokFor
	TokInput
	TokOutput
	TokSteps
	TokSql
	TokTouches
	TokInvalidate
	TokInsert
	TokArgPlaceholder // a $arg reference inside a raw SQL block

	// DSL constructs for declarative jobs. A `job` is a typed
	// background-work declaration; the args block lists typed inputs the
	// handler receives, retries / timeout / queue / schedule are runtime
	// modifiers. Workers and the typed client SDK are emitted from
	// these declarations by the codegen pipeline.
	TokJob
	TokArgs
	TokSchedule
	TokQueue
	TokRetries
	TokTimeout
	TokHeartbeat

	// TokEnqueue marks the `enqueue <Job>(args...)` step that appears
	// inside a procedure body. Atomic with the procedure transaction:
	// the job row INSERT shares the surrounding tx so the side effect
	// either commits with the procedure or rolls back with it.
	TokEnqueue

	// TokVisibleTo gates which callers can submit a job.
	TokVisibleTo

	// TokTtlField marks an entity-level `ttl_field <col>` directive.
	// Rows whose named column is in the past are swept by the
	// built-in SweepExpired scheduled job.
	TokTtlField

	// Workflow orchestration tokens.
	TokWorkflow   // `workflow <Name> in <ns> { ... }`
	TokStep       // `step <name> { job <Job>; args { ... } }`
	TokCompensate // `compensate <step-name> { job <Job>; args { ... } }`
	TokState      // `state { ... }` — typed workflow-level inputs

	// TokEphemeral declares a memcached-only data shape with a TTL.
	// No Postgres table, no migration, no VACUUM. Codegen emits typed
	// Get/Set/Delete backed by the atlantis memcached client.
	TokEphemeral
)

var tokenNames = map[TokenKind]string{
	TokError:          "ERROR",
	TokEOF:            "EOF",
	TokIdent:          "IDENT",
	TokInt:            "INT",
	TokFloat:          "FLOAT",
	TokString:         "STRING",
	TokDuration:       "DURATION",
	TokLBrace:         "{",
	TokRBrace:         "}",
	TokLParen:         "(",
	TokRParen:         ")",
	TokLBracket:       "[",
	TokRBracket:       "]",
	TokComma:          ",",
	TokColon:          ":",
	TokDColon:         "::",
	TokEquals:         "=",
	TokNotEq:          "!=",
	TokLT:             "<",
	TokLE:             "<=",
	TokGT:             ">",
	TokGE:             ">=",
	TokDot:            ".",
	TokEntity:         "entity",
	TokHypertable:     "hypertable",
	TokIn:             "in",
	TokPrimary:        "primary",
	TokIdentity:       "identity",
	TokSerial:         "serial",
	TokNot:            "not",
	TokNull:           "null",
	TokUnique:         "unique",
	TokDefault:        "default",
	TokCheck:          "check",
	TokBackfill:       "backfill",
	TokReferences:     "references",
	TokHasMany:        "has_many",
	TokHasOne:         "has_one",
	TokVia:            "via",
	TokIndex:          "index",
	TokBy:             "by",
	TokOn:             "on",
	TokHnsw:           "hnsw",
	TokOps:            "ops",
	TokCosine:         "cosine",
	TokL2:             "l2",
	TokIp:             "ip",
	TokGin:            "gin",
	TokPartial:        "partial",
	TokWhere:          "where",
	TokIs:             "is",
	TokAsc:            "asc",
	TokDesc:           "desc",
	TokCache:          "cache",
	TokReadThrough:    "read_through",
	TokTtl:            "ttl",
	TokTag:            "tag",
	TokInvalidateOn:   "invalidate_on",
	TokWrite:          "write",
	TokSelf:           "self",
	TokConsistency:    "consistency",
	TokStrict:         "strict",
	TokEventual:       "eventual",
	TokQueryTimeout:   "query_timeout",
	TokSoftDelete:     "soft_delete",
	TokTouchOnUpdate:  "touch_on_update",
	TokPartition:      "partition",
	TokTable:          "table",
	TokExpr:           "expr",
	TokAs:             "as",
	TokDeferrable:     "deferrable",
	TokTrue:           "true",
	TokFalse:          "false",
	TokNow:            "now",
	TokRaw:            "raw",
	TokDelete:         "delete",
	TokUpdate:         "update",
	TokCascade:        "cascade",
	TokRestrict:       "restrict",
	TokSet:            "set",
	TokQuery:          "query",
	TokProcedure:      "procedure",
	TokFor:            "for",
	TokInput:          "input",
	TokOutput:         "output",
	TokSteps:          "steps",
	TokSql:            "sql",
	TokTouches:        "touches",
	TokInvalidate:     "invalidate",
	TokInsert:         "insert",
	TokArgPlaceholder: "$ARG",
	TokJob:            "job",
	TokArgs:           "args",
	TokSchedule:       "schedule",
	TokQueue:          "queue",
	TokRetries:        "retries",
	TokTimeout:        "timeout",
	TokHeartbeat:      "heartbeat",
	TokEnqueue:        "enqueue",
	TokVisibleTo:      "visible_to",
	TokTtlField:       "ttl_field",
	TokWorkflow:       "workflow",
	TokStep:           "step",
	TokCompensate:     "compensate",
	TokState:          "state",
	TokEphemeral:      "ephemeral",
}

// String returns the textual form of the token kind.
func (k TokenKind) String() string {
	if name, ok := tokenNames[k]; ok {
		return name
	}
	return fmt.Sprintf("Token(%d)", int(k))
}

// keywords maps every reserved identifier-shaped word to its token kind.
// Anything not present here that lexes as Ident stays Ident.
var keywords = map[string]TokenKind{
	"entity":          TokEntity,
	"hypertable":      TokHypertable,
	"in":              TokIn,
	"primary":         TokPrimary,
	"identity":        TokIdentity,
	"serial":          TokSerial,
	"not":             TokNot,
	"null":            TokNull,
	"unique":          TokUnique,
	"default":         TokDefault,
	"check":           TokCheck,
	"backfill":        TokBackfill,
	"references":      TokReferences,
	"has_many":        TokHasMany,
	"has_one":         TokHasOne,
	"via":             TokVia,
	"index":           TokIndex,
	"by":              TokBy,
	"on":              TokOn,
	"hnsw":            TokHnsw,
	"ops":             TokOps,
	"cosine":          TokCosine,
	"l2":              TokL2,
	"ip":              TokIp,
	"gin":             TokGin,
	"partial":         TokPartial,
	"where":           TokWhere,
	"is":              TokIs,
	"asc":             TokAsc,
	"desc":            TokDesc,
	"cache":           TokCache,
	"read_through":    TokReadThrough,
	"ttl":             TokTtl,
	"tag":             TokTag,
	"invalidate_on":   TokInvalidateOn,
	"write":           TokWrite,
	"self":            TokSelf,
	"consistency":     TokConsistency,
	"strict":          TokStrict,
	"eventual":        TokEventual,
	"query_timeout":   TokQueryTimeout,
	"soft_delete":     TokSoftDelete,
	"touch_on_update": TokTouchOnUpdate,
	"partition":       TokPartition,
	"table":           TokTable,
	"expr":            TokExpr,
	"as":              TokAs,
	"deferrable":      TokDeferrable,
	"true":            TokTrue,
	"false":           TokFalse,
	"now":             TokNow,
	"raw":             TokRaw,
	"delete":          TokDelete,
	"update":          TokUpdate,
	"cascade":         TokCascade,
	"restrict":        TokRestrict,
	"set":             TokSet,
	"query":           TokQuery,
	"procedure":       TokProcedure,
	"for":             TokFor,
	"input":           TokInput,
	"output":          TokOutput,
	"steps":           TokSteps,
	"sql":             TokSql,
	"touches":         TokTouches,
	// `invalidate` only — the existing `invalidate_on` keyword stays
	// distinct because it appears inside cache blocks on entities, while
	// `invalidate` is the trailing clause on a procedure.
	"invalidate": TokInvalidate,
	"insert":     TokInsert,
	"job":        TokJob,
	"args":       TokArgs,
	"schedule":   TokSchedule,
	"queue":      TokQueue,
	"retries":    TokRetries,
	"timeout":    TokTimeout,
	"heartbeat":  TokHeartbeat,
	"enqueue":    TokEnqueue,
	"visible_to": TokVisibleTo,
	"ttl_field":  TokTtlField,
	"workflow":   TokWorkflow,
	"step":       TokStep,
	"compensate": TokCompensate,
	"state":      TokState,
	"ephemeral":  TokEphemeral,
}

// Position is a 1-indexed source position used for error reporting,
// plus a 0-indexed byte offset into the original source. The byte
// offset is the required piece for raw-SQL capture: when the parser
// sees `sql touches(...) { ... }`, it slices the raw body straight out
// of the original source using `src[lbrace.Pos.Byte+1 : rbrace.Pos.Byte]`
// rather than re-stringifying lexed tokens, so whitespace, casing, and
// SQL comments survive unchanged through to pg_query_go validation.
type Position struct {
	File string
	Line int
	Col  int
	Byte int // 0-indexed byte offset into the source; 0 for synthesized positions
}

func (p Position) String() string {
	if p.File != "" {
		return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col)
	}
	return fmt.Sprintf("%d:%d", p.Line, p.Col)
}

// Token is one lexer output. Value holds the literal text for IDENT / INT /
// STRING / DURATION; for keywords and punctuation Value is the form.
type Token struct {
	Kind  TokenKind
	Value string
	Pos   Position
}

func (t Token) String() string {
	if t.Value != "" && t.Value != t.Kind.String() {
		return fmt.Sprintf("%s(%q) @ %s", t.Kind, t.Value, t.Pos)
	}
	return fmt.Sprintf("%s @ %s", t.Kind, t.Pos)
}
