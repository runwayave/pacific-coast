package dsl

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
)

// Parser turns a token stream into an AST File.
//
// Hand-written recursive descent. Accumulates errors so a single parse call
// can surface multiple problems per file rather than bailing on the first.
type Parser struct {
	toks []Token
	pos  int
	errs []error
	// src holds the original source bytes so the parser can slice raw
	// SQL out of `sql { ... }` blocks verbatim. Position.Byte on the
	// LBRACE / RBRACE tokens gives the boundary; everything in between
	// (whitespace, SQL comments, casing) survives the trip to
	// pg_query_go at IR-lowering time. Empty for synthesized parses.
	src []byte
}

// Parse parses src as one .atl file. file is used for error messages.
// Returns the AST even on error; errors are reported via the returned error
// (which may be a multi-error if multiple problems were found).
func Parse(file string, src []byte) (*File, error) {
	toks := NewLexer(file, src).Lex()
	// Surface lexer errors before we try to parse.
	var lexErrs []error
	clean := make([]Token, 0, len(toks))
	for _, t := range toks {
		if t.Kind == TokError {
			lexErrs = append(lexErrs, fmt.Errorf("%s: lex error: %s", t.Pos, t.Value))
			continue
		}
		clean = append(clean, t)
	}
	p := &Parser{toks: clean, src: src}
	f := p.parseFile()
	f.Path = file

	allErrs := append(lexErrs, p.errs...)
	if len(allErrs) > 0 {
		return f, errors.Join(allErrs...)
	}
	return f, nil
}

// ---- cursor helpers ----

func (p *Parser) peek() Token { return p.toks[p.pos] }

func (p *Parser) advance() Token {
	t := p.toks[p.pos]
	if t.Kind != TokEOF {
		p.pos++
	}
	return t
}

// expect consumes a token of the given kind or records an error.
// Returns the token (which may be a synthetic error token if missing).
func (p *Parser) expect(kind TokenKind) Token {
	t := p.peek()
	if t.Kind != kind {
		p.errf(t.Pos, "expected %s, got %s", kind, t.Kind)
		return Token{Kind: TokError, Pos: t.Pos}
	}
	return p.advance()
}

// accept consumes the next token if it matches one of kinds, otherwise no-op.
// Returns true and the token if matched.
func (p *Parser) accept(kinds ...TokenKind) (Token, bool) {
	t := p.peek()
	if slices.Contains(kinds, t.Kind) {
		p.advance()
		return t, true
	}
	return Token{}, false
}

func (p *Parser) errf(pos Position, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: parse error: %s", pos, fmt.Sprintf(format, args...)))
}

// recover advances until we hit one of the synchronization tokens, so the
// parser can keep trying after a localized error.
func (p *Parser) recover(syncs ...TokenKind) {
	for {
		t := p.peek()
		if t.Kind == TokEOF {
			return
		}
		if slices.Contains(syncs, t.Kind) {
			return
		}
		p.advance()
	}
}

// ---- file / top-level ----

func (p *Parser) parseFile() *File {
	f := &File{}
	for {
		t := p.peek()
		switch t.Kind {
		case TokEOF:
			return f
		case TokEntity:
			if d := p.parseEntity(); d != nil {
				f.Decls = append(f.Decls, d)
			}
		case TokHypertable:
			if d := p.parseHypertable(); d != nil {
				f.Decls = append(f.Decls, d)
			}
		case TokQuery:
			if d := p.parseQuery(); d != nil {
				f.Decls = append(f.Decls, d)
			}
		case TokProcedure:
			if d := p.parseProcedure(); d != nil {
				f.Decls = append(f.Decls, d)
			}
		default:
			p.errf(t.Pos, "expected 'entity', 'hypertable', 'query', or 'procedure', got %s", t.Kind)
			p.recover(TokEntity, TokHypertable, TokQuery, TokProcedure)
		}
	}
}

func (p *Parser) parseEntity() *EntityDecl {
	kw := p.expect(TokEntity)
	if kw.Kind == TokError {
		p.recover(TokEntity, TokHypertable)
		return nil
	}
	name := p.expect(TokIdent)
	p.expect(TokIn)
	ns := p.expect(TokIdent)
	p.expect(TokLBrace)
	members := p.parseEntityMembers()
	p.expect(TokRBrace)
	return &EntityDecl{
		Pos:       kw.Pos,
		Name:      name.Value,
		Namespace: ns.Value,
		Members:   members,
	}
}

func (p *Parser) parseHypertable() *HypertableDecl {
	kw := p.expect(TokHypertable)
	if kw.Kind == TokError {
		p.recover(TokEntity, TokHypertable)
		return nil
	}
	name := p.expect(TokIdent)
	p.expect(TokIn)
	ns := p.expect(TokIdent)
	p.expect(TokOn)
	time := p.expect(TokIdent)
	p.expect(TokLBrace)
	members := p.parseEntityMembers()
	p.expect(TokRBrace)
	return &HypertableDecl{
		Pos:       kw.Pos,
		Name:      name.Value,
		Namespace: ns.Value,
		TimeField: time.Value,
		Members:   members,
	}
}

// ---- entity members ----

func (p *Parser) parseEntityMembers() []EntityMember {
	var members []EntityMember
	for {
		t := p.peek()
		switch t.Kind {
		case TokRBrace, TokEOF:
			return members
		case TokHasMany, TokHasOne:
			if m := p.parseRelation(); m != nil {
				members = append(members, m)
			}
		case TokIndex:
			if m := p.parseIndex(); m != nil {
				members = append(members, m)
			}
		case TokUnique:
			// Top-level "unique by a, b" — composite UNIQUE. Differs from
			// the field-modifier `unique` which only appears mid-field.
			if m := p.parseUniqueDecl(); m != nil {
				members = append(members, m)
			}
		case TokPrimary:
			// Top-level "primary by a, b" — composite PRIMARY KEY. The
			// field-modifier `primary` only appears mid-field (after a
			// type), so at member-start it must be the composite form.
			if m := p.parsePrimaryDecl(); m != nil {
				members = append(members, m)
			}
		case TokCheck:
			// Top-level `check "<expr>" [as name]` — table-level CHECK.
			// The field-modifier form of `check` is only reached inside
			// parseFieldModifiers; at member-start this is the table-level
			// form. Routes here regardless because by definition we're not
			// already mid-field.
			if m := p.parseTableCheckDecl(); m != nil {
				members = append(members, m)
			}
		case TokCache:
			if m := p.parseCacheBlock(); m != nil {
				members = append(members, m)
			}
		case TokQueryTimeout:
			if m := p.parseQueryTimeout(); m != nil {
				members = append(members, m)
			}
		case TokSoftDelete:
			if m := p.parseSoftDeleteDecl(); m != nil {
				members = append(members, m)
			}
		case TokTouchOnUpdate:
			if m := p.parseTouchOnUpdateDecl(); m != nil {
				members = append(members, m)
			}
		case TokPartition:
			if m := p.parsePartitionByDecl(); m != nil {
				members = append(members, m)
			}
		case TokIdent:
			if m := p.parseField(); m != nil {
				members = append(members, m)
			}
		default:
			p.errf(t.Pos, "unexpected token %s in entity body", t.Kind)
			// Sync to a likely member start so we can keep going.
			p.recover(TokRBrace, TokIdent, TokIndex, TokCache, TokHasMany, TokHasOne, TokQueryTimeout, TokUnique)
		}
	}
}

func (p *Parser) parseUniqueDecl() *UniqueDecl {
	kw := p.expect(TokUnique)
	p.expect(TokBy)
	u := &UniqueDecl{Pos: kw.Pos}
	for {
		name := p.expect(TokIdent)
		u.Fields = append(u.Fields, name.Value)
		if _, ok := p.accept(TokComma); !ok {
			// Field list done. The optional `deferrable` suffix lives here.
			if _, ok := p.accept(TokDeferrable); ok {
				u.Deferrable = true
			}
			return u
		}
	}
}

func (p *Parser) parseTableCheckDecl() *TableCheckDecl {
	kw := p.expect(TokCheck)
	expr := p.expect(TokString)
	c := &TableCheckDecl{Pos: kw.Pos, Expr: expr.Value}
	if _, ok := p.accept(TokAs); ok {
		name := p.expect(TokIdent)
		c.Name = name.Value
	}
	return c
}

func (p *Parser) parseSoftDeleteDecl() *SoftDeleteDecl {
	kw := p.expect(TokSoftDelete)
	p.expect(TokBy)
	field := p.expect(TokIdent)
	return &SoftDeleteDecl{Pos: kw.Pos, Field: field.Value}
}

func (p *Parser) parseTouchOnUpdateDecl() *TouchOnUpdateDecl {
	kw := p.expect(TokTouchOnUpdate)
	p.expect(TokBy)
	field := p.expect(TokIdent)
	return &TouchOnUpdateDecl{Pos: kw.Pos, Field: field.Value}
}

func (p *Parser) parsePartitionByDecl() *PartitionByDecl {
	kw := p.expect(TokPartition)
	p.expect(TokBy)
	field := p.expect(TokIdent)
	return &PartitionByDecl{Pos: kw.Pos, Field: field.Value}
}

func (p *Parser) parsePrimaryDecl() *PrimaryDecl {
	kw := p.expect(TokPrimary)
	p.expect(TokBy)
	pd := &PrimaryDecl{Pos: kw.Pos}
	for {
		name := p.expect(TokIdent)
		pd.Fields = append(pd.Fields, name.Value)
		if _, ok := p.accept(TokComma); !ok {
			return pd
		}
	}
}

// ---- field ----

func (p *Parser) parseField() *FieldDecl {
	name := p.expect(TokIdent)
	typ := p.parseType()
	mods := p.parseFieldModifiers()
	return &FieldDecl{
		Pos:       name.Pos,
		Name:      name.Value,
		Type:      typ,
		Modifiers: mods,
	}
}

func (p *Parser) parseType() TypeRef {
	t := p.peek()

	// Array form: `[]Type`
	if t.Kind == TokLBracket {
		open := p.advance()
		p.expect(TokRBracket)
		inner := p.parseType()
		return TypeRef{
			Pos:   open.Pos,
			Name:  inner.Name,
			Array: true,
			Elem:  &inner,
		}
	}

	if t.Kind != TokIdent {
		p.errf(t.Pos, "expected type, got %s", t.Kind)
		p.advance()
		return TypeRef{Pos: t.Pos, Name: "<error>"}
	}
	p.advance()
	ref := TypeRef{Pos: t.Pos, Name: t.Value}

	switch t.Value {
	case "vector":
		p.expect(TokLParen)
		dim := p.expect(TokInt)
		p.expect(TokRParen)
		if n, err := strconv.Atoi(dim.Value); err == nil {
			ref.VecDim = n
		}
	case "varchar":
		p.expect(TokLParen)
		ln := p.expect(TokInt)
		p.expect(TokRParen)
		if n, err := strconv.Atoi(ln.Value); err == nil {
			ref.Len = n
		}
	case "numeric":
		// Optional precision/scale.
		if _, ok := p.accept(TokLParen); ok {
			pi := p.expect(TokInt)
			p.expect(TokComma)
			si := p.expect(TokInt)
			p.expect(TokRParen)
			if n, err := strconv.Atoi(pi.Value); err == nil {
				ref.NumP = n
			}
			if n, err := strconv.Atoi(si.Value); err == nil {
				ref.NumS = n
			}
			ref.HasNumP = true
		}
	}
	return ref
}

func (p *Parser) parseFieldModifiers() []FieldModifier {
	var mods []FieldModifier
	for {
		t := p.peek()
		switch t.Kind {
		case TokPrimary:
			// Disambiguate field-modifier `primary` from top-level composite
			// `primary by a, b`. If the next-next token is `by`, this is the
			// composite form: bail out of the modifier loop and let the
			// dispatcher route to parsePrimaryDecl.
			if p.pos+1 < len(p.toks) && p.toks[p.pos+1].Kind == TokBy {
				return mods
			}
			p.advance()
			mods = append(mods, &ModPrimaryDecl{Pos: t.Pos})
		case TokIdentity:
			p.advance()
			mods = append(mods, &ModIdentityDecl{Pos: t.Pos})
		case TokSerial:
			p.advance()
			mods = append(mods, &ModSerialDecl{Pos: t.Pos})
		case TokNot:
			p.advance()
			p.expect(TokNull)
			mods = append(mods, &ModNotNullDecl{Pos: t.Pos})
		case TokUnique:
			// Same disambiguation as primary: `unique by a, b` at member
			// position is a composite UNIQUE, not a field modifier.
			if p.pos+1 < len(p.toks) && p.toks[p.pos+1].Kind == TokBy {
				return mods
			}
			p.advance()
			mods = append(mods, &ModUniqueDecl{Pos: t.Pos})
		case TokCheck:
			p.advance()
			expr := p.expect(TokString)
			mods = append(mods, &ModCheckDecl{Pos: t.Pos, Expr: expr.Value})
		case TokBackfill:
			p.advance()
			expr := p.expect(TokString)
			mods = append(mods, &ModBackfillDecl{Pos: t.Pos, Expr: expr.Value})
		case TokDefault:
			p.advance()
			val := p.parseDefaultValue(t.Pos)
			mods = append(mods, &ModDefaultDecl{Pos: t.Pos, Value: val})
		case TokReferences:
			mods = append(mods, p.parseReferences())
		default:
			return mods
		}
	}
}

func (p *Parser) parseDefaultValue(at Position) DefaultValue {
	t := p.peek()
	switch t.Kind {
	case TokString:
		p.advance()
		return DefaultValue{Pos: t.Pos, Kind: DefaultString, Str: t.Value}
	case TokInt:
		p.advance()
		n, _ := strconv.ParseInt(t.Value, 10, 64)
		return DefaultValue{Pos: t.Pos, Kind: DefaultInt, Int: n}
	case TokTrue:
		p.advance()
		return DefaultValue{Pos: t.Pos, Kind: DefaultBool, Bool: true}
	case TokFalse:
		p.advance()
		return DefaultValue{Pos: t.Pos, Kind: DefaultBool, Bool: false}
	case TokNow:
		p.advance()
		p.expect(TokLParen)
		p.expect(TokRParen)
		return DefaultValue{Pos: t.Pos, Kind: DefaultNow}
	case TokRaw:
		// `default raw "<sql>"` — verbatim SQL. The expression is checked
		// at migration time by Postgres; we do not try to parse it here.
		p.advance()
		expr := p.expect(TokString)
		return DefaultValue{Pos: t.Pos, Kind: DefaultRaw, Str: expr.Value}
	default:
		p.errf(at, "expected default value, got %s", t.Kind)
		return DefaultValue{Pos: at}
	}
}

func (p *Parser) parseReferences() *ModReferencesDecl {
	kw := p.expect(TokReferences)
	ns := p.expect(TokIdent)
	p.expect(TokDot)
	entity := p.expect(TokIdent)
	p.expect(TokDot)
	field := p.expect(TokIdent)
	m := &ModReferencesDecl{
		Pos:          kw.Pos,
		TargetNS:     ns.Value,
		TargetEntity: entity.Value,
		TargetField:  field.Value,
	}
	// Optional `on delete X` / `on update Y` — may appear in either order, at most one of each.
	for {
		if _, ok := p.accept(TokOn); !ok {
			break
		}
		switch p.peek().Kind {
		case TokDelete:
			p.advance()
			m.OnDelete = p.parseRefAction()
		case TokUpdate:
			p.advance()
			m.OnUpdate = p.parseRefAction()
		default:
			p.errf(p.peek().Pos, "expected 'delete' or 'update' after 'on', got %s", p.peek().Kind)
			return m
		}
	}
	return m
}

func (p *Parser) parseRefAction() RefAction {
	t := p.peek()
	switch t.Kind {
	case TokCascade:
		p.advance()
		return RefActionCascade
	case TokRestrict:
		p.advance()
		return RefActionRestrict
	case TokSet:
		p.advance()
		p.expect(TokNull)
		return RefActionSetNull
	default:
		p.errf(t.Pos, "expected cascade/restrict/set null, got %s", t.Kind)
		p.advance()
		return RefActionUnset
	}
}

// ---- relation ----

func (p *Parser) parseRelation() *RelationDecl {
	kw := p.advance() // has_many or has_one
	kind := RelHasMany
	if kw.Kind == TokHasOne {
		kind = RelHasOne
	}
	name := p.expect(TokIdent)
	p.expect(TokColon)
	target := p.expect(TokIdent)
	p.expect(TokVia)
	via := p.expect(TokIdent)
	return &RelationDecl{
		Pos:    kw.Pos,
		Kind:   kind,
		Name:   name.Value,
		Target: target.Value,
		Via:    via.Value,
	}
}

// ---- index ----

func (p *Parser) parseIndex() *IndexDecl {
	kw := p.expect(TokIndex)
	t := p.peek()

	switch t.Kind {
	case TokBy:
		p.advance()
		fields := p.parseIndexFields()
		return &IndexDecl{Pos: kw.Pos, Kind: IndexBtree, Fields: fields}

	case TokHnsw:
		p.advance()
		p.expect(TokOn)
		field := p.expect(TokIdent)
		p.expect(TokOps)
		ops := p.parseVecOps()
		return &IndexDecl{
			Pos:    kw.Pos,
			Kind:   IndexHNSW,
			Field:  field.Value,
			VecOps: ops,
		}

	case TokGin:
		p.advance()
		p.expect(TokOn)
		field := p.expect(TokIdent)
		return &IndexDecl{Pos: kw.Pos, Kind: IndexGIN, Field: field.Value}

	case TokPartial:
		p.advance()
		p.expect(TokBy)
		fields := p.parseIndexFields()
		p.expect(TokWhere)
		where := p.parsePartialPredicate()
		return &IndexDecl{Pos: kw.Pos, Kind: IndexPartial, Fields: fields, Where: where}

	default:
		p.errf(t.Pos, "expected 'by'/'hnsw'/'gin'/'partial' after 'index', got %s", t.Kind)
		return nil
	}
}

func (p *Parser) parseIndexFields() []IndexField {
	var out []IndexField
	for {
		var f IndexField
		// `expr "lower(email)"` form
		if _, ok := p.accept(TokExpr); ok {
			lit := p.expect(TokString)
			f.IsExpr = true
			f.Expr = lit.Value
		} else {
			name := p.expect(TokIdent)
			f.Name = name.Value
		}
		if _, ok := p.accept(TokDesc); ok {
			f.Desc = true
		} else if _, ok := p.accept(TokAsc); ok {
			f.Desc = false
		}
		out = append(out, f)
		if _, ok := p.accept(TokComma); !ok {
			return out
		}
	}
}

func (p *Parser) parseVecOps() VectorOps {
	t := p.peek()
	switch t.Kind {
	case TokCosine:
		p.advance()
		return VecOpsCosine
	case TokL2:
		p.advance()
		return VecOpsL2
	case TokIp:
		p.advance()
		return VecOpsIP
	default:
		p.errf(t.Pos, "expected cosine/l2/ip, got %s", t.Kind)
		p.advance()
		return VecOpsCosine
	}
}

func (p *Parser) parsePartialPredicate() *PartialPredicate {
	field := p.expect(TokIdent)
	pp := &PartialPredicate{Pos: field.Pos, Field: field.Value}
	// Two forms:
	//   field is [not] null
	//   field <op> <literal>
	t := p.peek()
	switch t.Kind {
	case TokIs:
		p.advance()
		if _, ok := p.accept(TokNot); ok {
			p.expect(TokNull)
			pp.IsNull = false
		} else {
			p.expect(TokNull)
			pp.IsNull = true
		}
	case TokEquals, TokNotEq, TokLT, TokLE, TokGT, TokGE:
		pp.Op = t.Value
		p.advance()
		pp.Literal = p.parseDefaultValue(t.Pos)
	default:
		p.errf(t.Pos, "expected 'is' or comparison operator, got %s", t.Kind)
	}
	return pp
}

// ---- cache ----

func (p *Parser) parseCacheBlock() *CacheBlock {
	kw := p.expect(TokCache)
	p.expect(TokLBrace)
	cb := &CacheBlock{Pos: kw.Pos}
	for {
		t := p.peek()
		switch t.Kind {
		case TokRBrace, TokEOF:
			p.expect(TokRBrace)
			return cb
		case TokReadThrough:
			p.advance()
			p.expect(TokTtl)
			p.expect(TokEquals)
			dur := p.expect(TokDuration)
			cb.HasReadThrough = true
			cb.TTL = dur.Value
			// Optional tag.
			if _, ok := p.accept(TokTag); ok {
				p.expect(TokEquals)
				tag := p.expect(TokString)
				cb.Tag = tag.Value
			}
		case TokInvalidateOn:
			p.advance()
			p.expect(TokColon)
			cb.Invalidate = append(cb.Invalidate, p.parseInvalidateList()...)
		case TokConsistency:
			p.advance()
			p.expect(TokEquals)
			cb.Consistency = p.parseConsistency()
		default:
			p.errf(t.Pos, "unexpected token %s in cache block", t.Kind)
			p.recover(TokRBrace, TokReadThrough, TokInvalidateOn, TokConsistency)
		}
	}
}

func (p *Parser) parseInvalidateList() []InvalidateClause {
	var out []InvalidateClause
	for {
		out = append(out, p.parseInvalidateClause())
		if _, ok := p.accept(TokComma); !ok {
			return out
		}
	}
}

func (p *Parser) parseInvalidateClause() InvalidateClause {
	kw := p.expect(TokWrite)
	p.expect(TokLParen)
	c := InvalidateClause{Pos: kw.Pos}
	t := p.peek()
	if t.Kind == TokSelf {
		p.advance()
		c.Self = true
		p.expect(TokRParen)
		return c
	}
	target := p.expect(TokIdent)
	c.Target = target.Value
	if _, ok := p.accept(TokWhere); ok {
		fld := p.expect(TokIdent)
		p.expect(TokEquals)
		p.expect(TokSelf)
		p.expect(TokDot)
		sf := p.expect(TokIdent)
		c.Where = &InvalidateWhere{Field: fld.Value, SelfField: sf.Value}
	}
	p.expect(TokRParen)
	return c
}

func (p *Parser) parseConsistency() Consistency {
	t := p.peek()
	switch t.Kind {
	case TokStrict:
		p.advance()
		return ConsistencyStrict
	case TokEventual:
		p.advance()
		return ConsistencyEventual
	default:
		p.errf(t.Pos, "expected strict/eventual, got %s", t.Kind)
		p.advance()
		return ConsistencyDefault
	}
}

// ---- query_timeout ----

func (p *Parser) parseQueryTimeout() *QueryTimeoutDecl {
	kw := p.expect(TokQueryTimeout)
	p.expect(TokEquals)
	dur := p.expect(TokDuration)
	return &QueryTimeoutDecl{Pos: kw.Pos, Duration: dur.Value}
}

// ---- Custom queries and procedures ----
//
// The grammar is one straight-line parse per construct: the only
// production with real ambiguity is the typed-step WHERE expression,
// which is parsed via parseExpr below. Raw SQL blocks come back as
// pre-captured TokString tokens (see lexer.captureRawSQLBody) so the
// parser doesn't have to scan them character by character.

// parseQuery: `query Name for [ns.]Entity { input { ... } output { ... } sql touches(...) { ... } cache { ... }? }`.
func (p *Parser) parseQuery() *QueryDecl {
	kw := p.expect(TokQuery)
	if kw.Kind == TokError {
		p.recover(TokEntity, TokHypertable, TokQuery, TokProcedure)
		return nil
	}
	name := p.expect(TokIdent)
	p.expect(TokFor)
	target := p.parseEntityRef()
	p.expect(TokLBrace)

	q := &QueryDecl{Pos: kw.Pos, Name: name.Value, Target: target}
	for {
		t := p.peek()
		switch t.Kind {
		case TokRBrace, TokEOF:
			p.expect(TokRBrace)
			return q
		case TokInput:
			q.Inputs = p.parseInputBlock()
		case TokOutput:
			q.Output = p.parseQueryOutput()
		case TokSql:
			q.SQL = p.parseSQLBlock()
		case TokCache:
			q.Cache = p.parseCacheBlock()
		default:
			p.errf(t.Pos, "unexpected %s in query body", t.Kind)
			p.recover(TokRBrace, TokInput, TokOutput, TokSql, TokCache)
		}
	}
}

// parseProcedure: `procedure Name for [ns.]Entity { input { ... } steps { ... } invalidate: tag("...")? }`.
func (p *Parser) parseProcedure() *ProcedureDecl {
	kw := p.expect(TokProcedure)
	if kw.Kind == TokError {
		p.recover(TokEntity, TokHypertable, TokQuery, TokProcedure)
		return nil
	}
	name := p.expect(TokIdent)
	p.expect(TokFor)
	target := p.parseEntityRef()
	p.expect(TokLBrace)

	pd := &ProcedureDecl{Pos: kw.Pos, Name: name.Value, Target: target}
	for {
		t := p.peek()
		switch t.Kind {
		case TokRBrace, TokEOF:
			p.expect(TokRBrace)
			return pd
		case TokInput:
			pd.Inputs = p.parseInputBlock()
		case TokSteps:
			pd.Steps = p.parseStepsBlock()
		case TokInvalidate:
			pd.Invalidate = p.parseProcedureInvalidate()
		default:
			p.errf(t.Pos, "unexpected %s in procedure body", t.Kind)
			p.recover(TokRBrace, TokInput, TokSteps, TokInvalidate)
		}
	}
}

// parseEntityRef reads `Name` or `Namespace.Name`. The namespace is
// optional; an unqualified name binds to the file's namespace at IR
// lowering time.
func (p *Parser) parseEntityRef() EntityRef {
	first := p.expect(TokIdent)
	ref := EntityRef{Pos: first.Pos, Name: first.Value}
	if _, ok := p.accept(TokDot); ok {
		second := p.expect(TokIdent)
		ref.Namespace = first.Value
		ref.Name = second.Value
	}
	return ref
}

// parseInputBlock: `input { name1: type1, name2: type2 default value, ... }`.
// Trailing commas are optional. Defaults reuse the entity-field default
// parser to keep the surface uniform — a default of `now` produces a
// LiteralExpr{Kind:"now"}, a string literal becomes Kind:"string", etc.
func (p *Parser) parseInputBlock() []InputParam {
	p.expect(TokInput)
	p.expect(TokLBrace)
	var out []InputParam
	for {
		t := p.peek()
		if t.Kind == TokRBrace || t.Kind == TokEOF {
			p.expect(TokRBrace)
			return out
		}
		name := p.expect(TokIdent)
		p.expect(TokColon)
		typ := p.parseType()
		param := InputParam{Pos: name.Pos, Name: name.Value, Type: typ}
		if _, ok := p.accept(TokDefault); ok {
			param.Default = p.parseDefaultExpr()
		}
		out = append(out, param)
		// Comma-separated; trailing comma OK.
		if _, ok := p.accept(TokComma); !ok {
			// Either next is }, or it's another field on its own line.
			if p.peek().Kind == TokRBrace {
				continue
			}
		}
	}
}

// parseQueryOutput: either `output as <Entity>` or `output { col: type, ... }`.
func (p *Parser) parseQueryOutput() *QueryOutput {
	kw := p.expect(TokOutput)
	out := &QueryOutput{Pos: kw.Pos}
	if _, ok := p.accept(TokAs); ok {
		ref := p.parseEntityRef()
		out.AsEntity = &ref
		return out
	}
	p.expect(TokLBrace)
	for {
		t := p.peek()
		if t.Kind == TokRBrace || t.Kind == TokEOF {
			p.expect(TokRBrace)
			return out
		}
		name := p.expect(TokIdent)
		p.expect(TokColon)
		typ := p.parseType()
		out.Columns = append(out.Columns, InputParam{Pos: name.Pos, Name: name.Value, Type: typ})
		if _, ok := p.accept(TokComma); !ok {
			if p.peek().Kind == TokRBrace {
				continue
			}
		}
	}
}

// parseSQLBlock: `sql touches(E1, E2, ...) { ... raw SQL ... }`. The
// lexer captured the raw body as a single TokString between LBRACE and
// RBRACE (see lexer.captureRawSQLBody), so the parser just unwraps it.
func (p *Parser) parseSQLBlock() *SQLBlock {
	kw := p.expect(TokSql)
	blk := &SQLBlock{Pos: kw.Pos}
	blk.Touches = p.parseTouchesClause()
	lbrace := p.expect(TokLBrace)
	blk.Pos = lbrace.Pos
	body := p.expect(TokString)
	blk.Raw = body.Value
	rbrace := p.expect(TokRBrace)
	blk.EndPos = rbrace.Pos
	return blk
}

// parseTouchesClause: `touches(Entity1, ns.Entity2, ...)`. Required —
// the entity list is what the cache-invalidation layer reads to know
// which generation counters to bump when this SQL writes data.
func (p *Parser) parseTouchesClause() []EntityRef {
	p.expect(TokTouches)
	p.expect(TokLParen)
	var refs []EntityRef
	for {
		refs = append(refs, p.parseEntityRef())
		if _, ok := p.accept(TokComma); !ok {
			break
		}
	}
	p.expect(TokRParen)
	return refs
}

// parseStepsBlock: `steps { ProcStep+ }`.
func (p *Parser) parseStepsBlock() []ProcedureStep {
	p.expect(TokSteps)
	p.expect(TokLBrace)
	var out []ProcedureStep
	for {
		t := p.peek()
		switch t.Kind {
		case TokRBrace, TokEOF:
			p.expect(TokRBrace)
			return out
		case TokUpdate, TokDelete, TokInsert:
			step := p.parseTypedStep()
			if step != nil {
				out = append(out, ProcedureStep{Pos: step.Pos, Typed: step})
			}
		case TokSql:
			blk := p.parseSQLBlock()
			if blk != nil {
				out = append(out, ProcedureStep{Pos: blk.Pos, Raw: blk})
			}
		default:
			p.errf(t.Pos, "expected update/delete/insert/sql in steps block, got %s", t.Kind)
			p.recover(TokRBrace, TokUpdate, TokDelete, TokInsert, TokSql)
		}
	}
}

// parseTypedStep: `update Entity set f = v[, ...] where expr` |
// `delete Entity where expr` | `insert Entity { f = v, ... }`.
// The where/set expression grammar is narrow (literal,
// arg, field, equality, AND); richer predicates belong in raw SQL.
func (p *Parser) parseTypedStep() *TypedStep {
	verbTok := p.advance() // TokUpdate, TokDelete, or TokInsert
	step := &TypedStep{Pos: verbTok.Pos, Verb: verbTok.Value}
	step.Target = p.parseEntityRef()
	switch verbTok.Kind {
	case TokUpdate:
		p.expect(TokSet)
		for {
			step.Assigns = append(step.Assigns, p.parseAssignment())
			if _, ok := p.accept(TokComma); !ok {
				break
			}
		}
		p.expect(TokWhere)
		step.WhereExpr = p.parseExpr()
	case TokDelete:
		p.expect(TokWhere)
		step.WhereExpr = p.parseExpr()
	case TokInsert:
		p.expect(TokLBrace)
		for {
			if p.peek().Kind == TokRBrace {
				p.expect(TokRBrace)
				break
			}
			step.Assigns = append(step.Assigns, p.parseAssignment())
			if _, ok := p.accept(TokComma); !ok {
				p.expect(TokRBrace)
				break
			}
		}
	}
	return step
}

// parseAssignment: `field = expr`.
func (p *Parser) parseAssignment() SetAssignment {
	field := p.expect(TokIdent)
	p.expect(TokEquals)
	val := p.parseExpr()
	return SetAssignment{Pos: field.Pos, Field: field.Value, Value: val}
}

// parseExpr parses a typed-step expression. The grammar is small:
//
//	Expr      := Cmp ( "and" Cmp )*
//	Cmp       := Atom ( ("=" | "!=" | "<" | "<=" | ">" | ">=") Atom )?
//	Atom      := ArgRef | Literal | FieldRef | "now" "(" ")"
//
// `and` reuses the existing TokIdent lexing — we don't have an AND
// token because the entity DSL never needed one. Comparisons are
// non-associative; AND is left-associative. Anything richer (OR, NOT,
// nested parens) is a sign the step belongs in a raw SQL block.
func (p *Parser) parseExpr() Expr {
	left := p.parseCmp()
	for {
		t := p.peek()
		if t.Kind != TokIdent || t.Value != "and" {
			return left
		}
		opTok := p.advance()
		right := p.parseCmp()
		left = &BinaryExpr{Pos: opTok.Pos, Op: "and", Left: left, Right: right}
	}
}

func (p *Parser) parseCmp() Expr {
	left := p.parseAtom()
	t := p.peek()
	switch t.Kind {
	case TokEquals, TokNotEq, TokLT, TokLE, TokGT, TokGE:
		opTok := p.advance()
		right := p.parseAtom()
		return &BinaryExpr{Pos: opTok.Pos, Op: opTok.Value, Left: left, Right: right}
	}
	return left
}

func (p *Parser) parseAtom() Expr {
	t := p.peek()
	switch t.Kind {
	case TokArgPlaceholder:
		p.advance()
		return &ArgExpr{Pos: t.Pos, Name: t.Value}
	case TokInt:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "int", Value: t.Value}
	case TokString:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "string", Value: t.Value}
	case TokTrue, TokFalse:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "bool", Value: t.Value}
	case TokNow:
		p.advance()
		// `now()` — the parens are optional in the entity grammar; keep
		// them optional here for symmetry.
		if _, ok := p.accept(TokLParen); ok {
			p.expect(TokRParen)
		}
		return &LiteralExpr{Pos: t.Pos, Kind: "now", Value: "now"}
	case TokIdent:
		p.advance()
		return &FieldExpr{Pos: t.Pos, Name: t.Value}
	default:
		p.errf(t.Pos, "expected expression, got %s", t.Kind)
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "int", Value: "0"}
	}
}

// parseProcedureInvalidate: `invalidate: tag("template")`.
func (p *Parser) parseProcedureInvalidate() *ProcedureInvalidate {
	kw := p.expect(TokInvalidate)
	p.expect(TokColon)
	p.expect(TokTag)
	p.expect(TokLParen)
	tpl := p.expect(TokString)
	p.expect(TokRParen)
	return &ProcedureInvalidate{Pos: kw.Pos, TagTpl: tpl.Value}
}

// parseDefaultExpr reads the value following `default` in an input
// parameter declaration. Mirrors the entity-field default parser but
// returns an Expr (which carries position) instead of a parsed string.
func (p *Parser) parseDefaultExpr() Expr {
	t := p.peek()
	switch t.Kind {
	case TokInt:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "int", Value: t.Value}
	case TokString:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "string", Value: t.Value}
	case TokTrue, TokFalse:
		p.advance()
		return &LiteralExpr{Pos: t.Pos, Kind: "bool", Value: t.Value}
	case TokNow:
		p.advance()
		if _, ok := p.accept(TokLParen); ok {
			p.expect(TokRParen)
		}
		return &LiteralExpr{Pos: t.Pos, Kind: "now", Value: "now"}
	default:
		p.errf(t.Pos, "expected default value, got %s", t.Kind)
		p.advance()
		return nil
	}
}
