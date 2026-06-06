package dsl

import (
	"strings"
	"testing"
)

func lexAll(t *testing.T, src string) []Token {
	t.Helper()
	return NewLexer("t.pc", []byte(src)).Lex()
}

// kinds extracts just the TokenKinds — convenient for assertion on shape.
func kinds(toks []Token) []TokenKind {
	out := make([]TokenKind, len(toks))
	for i, t := range toks {
		out[i] = t.Kind
	}
	return out
}

func kindsEq(a, b []TokenKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLex_EmptyInput(t *testing.T) {
	toks := lexAll(t, "")
	if len(toks) != 1 || toks[0].Kind != TokEOF {
		t.Fatalf("empty input should produce only EOF, got %v", toks)
	}
}

func TestLex_Whitespace(t *testing.T) {
	toks := lexAll(t, "   \t \n  \r\n  ")
	if len(toks) != 1 || toks[0].Kind != TokEOF {
		t.Fatalf("whitespace-only should produce only EOF, got %v", toks)
	}
}

func TestLex_LineComment(t *testing.T) {
	toks := lexAll(t, "// hello\nentity")
	if len(toks) != 2 {
		t.Fatalf("expected 2 tokens, got %d: %v", len(toks), toks)
	}
	if toks[0].Kind != TokEntity {
		t.Fatalf("expected TokEntity, got %v", toks[0])
	}
}

func TestLex_BlockComment(t *testing.T) {
	toks := lexAll(t, "/* multi\nline */entity")
	if toks[0].Kind != TokEntity {
		t.Fatalf("expected TokEntity, got %v", toks[0])
	}
}

func TestLex_Punctuation(t *testing.T) {
	toks := lexAll(t, "{ } ( ) [ ] , : = .")
	want := []TokenKind{
		TokLBrace, TokRBrace, TokLParen, TokRParen,
		TokLBracket, TokRBracket, TokComma, TokColon, TokEquals, TokDot,
		TokEOF,
	}
	if !kindsEq(kinds(toks), want) {
		t.Fatalf("kinds mismatch:\n got %v\nwant %v", kinds(toks), want)
	}
}

func TestLex_Keywords(t *testing.T) {
	src := `entity hypertable in primary not null unique default references
            has_many has_one via index by on hnsw ops cosine l2 ip gin partial
            where is asc desc cache read_through ttl tag invalidate_on write self
            consistency strict eventual query_timeout true false now
            delete update cascade restrict set
            query procedure for input output steps sql touches invalidate insert`
	toks := lexAll(t, src)
	// Every keyword should produce a non-Ident token.
	for _, tok := range toks {
		if tok.Kind == TokEOF {
			break
		}
		if tok.Kind == TokIdent {
			t.Fatalf("keyword %q lexed as TokIdent", tok.Value)
		}
	}
}

func TestLex_Identifiers(t *testing.T) {
	toks := lexAll(t, "foo Bar baz_qux _under x1 z123")
	for i, tok := range toks {
		if tok.Kind == TokEOF {
			break
		}
		if tok.Kind != TokIdent {
			t.Fatalf("token %d expected TokIdent, got %v", i, tok)
		}
	}
}

func TestLex_KeywordVsIdent_Boundary(t *testing.T) {
	// `entity_thing` should be a single Ident, not entity + Ident.
	toks := lexAll(t, "entity_thing")
	if toks[0].Kind != TokIdent || toks[0].Value != "entity_thing" {
		t.Fatalf("expected Ident(entity_thing), got %v", toks[0])
	}
}

func TestLex_Integers(t *testing.T) {
	toks := lexAll(t, "0 42 1000000")
	for _, tok := range toks {
		if tok.Kind == TokEOF {
			break
		}
		if tok.Kind != TokInt {
			t.Fatalf("expected TokInt, got %v", tok)
		}
	}
}

func TestLex_Durations(t *testing.T) {
	cases := []struct{ src, value string }{
		{"30s", "30s"},
		{"10m", "10m"},
		{"1h", "1h"},
		{"7d", "7d"},
		{"500ms", "500"}, // we do NOT lex `ms` as a duration suffix; "500" lexes as int, "ms" as ident
	}
	for _, c := range cases {
		toks := lexAll(t, c.src)
		if c.src == "500ms" {
			// special case: int + ident
			if toks[0].Kind != TokInt || toks[0].Value != "500" {
				t.Fatalf("%q: expected Int(500), got %v", c.src, toks[0])
			}
			if toks[1].Kind != TokIdent || toks[1].Value != "ms" {
				t.Fatalf("%q: expected Ident(ms), got %v", c.src, toks[1])
			}
			continue
		}
		if toks[0].Kind != TokDuration || toks[0].Value != c.value {
			t.Fatalf("%q: expected Duration(%s), got %v", c.src, c.value, toks[0])
		}
	}
}

func TestLex_IntWithIdentSuffix(t *testing.T) {
	// `10mins` should lex as Int(10) + Ident(mins). The duration suffix only
	// kicks in if not followed by an identifier rune.
	toks := lexAll(t, "10mins")
	if toks[0].Kind != TokInt || toks[0].Value != "10" {
		t.Fatalf("expected Int(10), got %v", toks[0])
	}
	if toks[1].Kind != TokIdent || toks[1].Value != "mins" {
		t.Fatalf("expected Ident(mins), got %v", toks[1])
	}
}

func TestLex_StringSimple(t *testing.T) {
	toks := lexAll(t, `"hello world"`)
	if toks[0].Kind != TokString || toks[0].Value != "hello world" {
		t.Fatalf("expected String(hello world), got %v", toks[0])
	}
}

func TestLex_StringWithEscapes(t *testing.T) {
	toks := lexAll(t, `"a\"b\nc\t\\d"`)
	want := "a\"b\nc\t\\d"
	if toks[0].Kind != TokString || toks[0].Value != want {
		t.Fatalf("expected String(%q), got %v", want, toks[0])
	}
}

func TestLex_StringUnterminated_EOF(t *testing.T) {
	toks := lexAll(t, `"unterminated`)
	if toks[0].Kind != TokError {
		t.Fatalf("expected TokError, got %v", toks[0])
	}
}

func TestLex_StringUnterminated_Newline(t *testing.T) {
	toks := lexAll(t, "\"oh no\nrest")
	if toks[0].Kind != TokError {
		t.Fatalf("expected TokError on newline-in-string, got %v", toks[0])
	}
}

func TestLex_UnknownEscape(t *testing.T) {
	toks := lexAll(t, `"\q"`)
	if toks[0].Kind != TokError {
		t.Fatalf("expected TokError, got %v", toks[0])
	}
}

func TestLex_Positions(t *testing.T) {
	src := "entity Foo\n  in consumer"
	toks := lexAll(t, src)
	// "entity" at 1:1, "Foo" at 1:8, "in" at 2:3, "consumer" at 2:6
	checks := []struct {
		idx       int
		line, col int
		kind      TokenKind
	}{
		{0, 1, 1, TokEntity},
		{1, 1, 8, TokIdent},
		{2, 2, 3, TokIn},
		{3, 2, 6, TokIdent},
	}
	for _, c := range checks {
		tok := toks[c.idx]
		if tok.Kind != c.kind {
			t.Errorf("tok[%d]: kind got %v want %v", c.idx, tok.Kind, c.kind)
		}
		if tok.Pos.Line != c.line || tok.Pos.Col != c.col {
			t.Errorf("tok[%d] (%v): pos got %d:%d want %d:%d",
				c.idx, tok.Kind, tok.Pos.Line, tok.Pos.Col, c.line, c.col)
		}
	}
}

// TestLex_ArgPlaceholder pins the shape lexer produces for `$name`
// references used inside procedure typed-step expressions. The leading
// `$` advances the cursor but does not appear in the token Value —
// callers read the bare name. A lone `$` or a `$` followed by anything
// that's not an identifier-start is a lex error so the parser surfaces
// the malformed input immediately instead of swallowing it as junk.
func TestLex_ArgPlaceholder(t *testing.T) {
	toks := lexAll(t, "$account_id $x $1bad $")
	// $account_id: TokArgPlaceholder, value "account_id"
	if toks[0].Kind != TokArgPlaceholder || toks[0].Value != "account_id" {
		t.Fatalf("expected ArgPlaceholder(account_id), got %v", toks[0])
	}
	// $x: TokArgPlaceholder, value "x"
	if toks[1].Kind != TokArgPlaceholder || toks[1].Value != "x" {
		t.Fatalf("expected ArgPlaceholder(x), got %v", toks[1])
	}
	// $1bad: TokError — identifier can't start with a digit.
	if toks[2].Kind != TokError {
		t.Fatalf("expected error on $1bad, got %v", toks[2])
	}
	// `$` alone before whitespace is an error. The error consumes the
	// `$`, then `1bad` lexes as integer + identifier.
	// The next token here is the literal int 1 from the recovery path.
	// We only check the trailing lone `$` produces a TokError.
	last := toks[len(toks)-2] // -1 is EOF, -2 is the previous token
	if last.Kind != TokError {
		t.Fatalf("expected lone $ to be an error, got %v", last)
	}
}

// TestLex_PositionByteOffset confirms every token carries the byte
// offset into the source. The parser depends on this to slice raw SQL
// out of `sql { ... }` bodies between the LBRACE and RBRACE positions
// without having to re-stringify the captured tokens (which would lose
// whitespace + comments that the SQL validator needs to see).
func TestLex_PositionByteOffset(t *testing.T) {
	src := "abc { def }"
	// Bytes: a=0 b=1 c=2 space=3 {=4 space=5 d=6 e=7 f=8 space=9 }=10
	toks := lexAll(t, src)
	if toks[0].Pos.Byte != 0 || toks[1].Pos.Byte != 4 || toks[2].Pos.Byte != 6 || toks[3].Pos.Byte != 10 {
		t.Fatalf("byte offsets: got %d,%d,%d,%d; want 0,4,6,10",
			toks[0].Pos.Byte, toks[1].Pos.Byte, toks[2].Pos.Byte, toks[3].Pos.Byte)
	}
}

func TestLex_FullEntity(t *testing.T) {
	src := `
entity SavedOutfit in consumer {
  id           bigint primary
  consumer_id  bigint references consumer.Account.id
  name         text not null
  created_at   timestamptz default now()

  has_many items: SavedOutfitItem via outfit_id
  index by consumer_id, created_at desc

  cache {
    read_through ttl=10m tag="consumer:{consumer_id}"
    invalidate_on: write(self), write(SavedOutfitItem where outfit_id = self.id)
  }
}
`
	toks := lexAll(t, src)
	// We don't pin the exact sequence — that's the parser's job to assert.
	// We just verify no TokError, and the file ends with EOF.
	for _, tok := range toks {
		if tok.Kind == TokError {
			t.Fatalf("unexpected lex error: %s at %s", tok.Value, tok.Pos)
		}
	}
	if toks[len(toks)-1].Kind != TokEOF {
		t.Fatalf("token stream should end with EOF")
	}
	// Sanity: the stream should contain the `read_through`, `cache`, `hnsw`-less
	// shape, and a string literal with a tag template.
	wantSeen := map[TokenKind]bool{
		TokEntity:      false,
		TokCache:       false,
		TokReadThrough: false,
		TokDuration:    false,
		TokString:      false,
	}
	for _, tok := range toks {
		if _, ok := wantSeen[tok.Kind]; ok {
			wantSeen[tok.Kind] = true
		}
	}
	for k, seen := range wantSeen {
		if !seen {
			t.Errorf("expected to see %v in token stream", k)
		}
	}
}

func TestLex_UnexpectedChar(t *testing.T) {
	toks := lexAll(t, "foo @ bar")
	// expect: Ident, Error, Ident, EOF
	if len(toks) != 4 {
		t.Fatalf("expected 4 tokens, got %d: %v", len(toks), toks)
	}
	if toks[1].Kind != TokError {
		t.Fatalf("expected TokError at index 1, got %v", toks[1])
	}
	if !strings.Contains(toks[1].Value, "unexpected") {
		t.Fatalf("error message should mention 'unexpected', got %q", toks[1].Value)
	}
}
