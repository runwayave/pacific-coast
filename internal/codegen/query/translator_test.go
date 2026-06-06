package query

import (
	"strings"
	"testing"
	"time"

	commonv1 "github.com/rachitkumar205/atlantis-go/pb/atlantis/common/v1"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ----------------------------------------------------------------------------
// Test fixture: a hand-built TestFilter message that mirrors what codegen
// the translator emits. Built via descriptorpb + protodesc so we
// don't need a buf-generated test proto cluttering the repo.

func testFilterDescriptor(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	// Match buf-generated layout: import the common/v1 file, declare
	// TestFilter with one optional <Type>Predicate per supported kind +
	// the three composite arms (and / or / not).
	fileProto := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("test.proto"),
		Package: proto.String("atlantis.test.v1"),
		Syntax:  proto.String("proto3"),
		Dependency: []string{
			"atlantis/common/v1/predicates.proto",
		},
		MessageType: []*descriptorpb.DescriptorProto{
			{
				Name: proto.String("TestFilter"),
				Field: []*descriptorpb.FieldDescriptorProto{
					optMsg(1, "id", ".atlantis.common.v1.Int64Predicate"),
					optMsg(2, "name", ".atlantis.common.v1.StringPredicate"),
					optMsg(3, "age", ".atlantis.common.v1.Int32Predicate"),
					optMsg(4, "active", ".atlantis.common.v1.BoolPredicate"),
					optMsg(5, "created_at", ".atlantis.common.v1.TimestampPredicate"),
					optMsg(6, "raw", ".atlantis.common.v1.BytesPredicate"),
					optMsg(7, "price", ".atlantis.common.v1.NumericPredicate"),
					repMsg(100, "and", ".atlantis.test.v1.TestFilter"),
					repMsg(101, "or", ".atlantis.test.v1.TestFilter"),
					optMsg(102, "not", ".atlantis.test.v1.TestFilter"),
				},
			},
		},
	}

	// Buf's FileDescriptorSet for the common proto. We build a FileSet
	// containing both files and resolve.
	commonProto := protodesc.ToFileDescriptorProto(commonv1.File_atlantis_common_v1_predicates_proto)
	tsProto := protodesc.ToFileDescriptorProto((&timestamppb.Timestamp{}).ProtoReflect().Descriptor().ParentFile())

	set := &descriptorpb.FileDescriptorSet{
		File: []*descriptorpb.FileDescriptorProto{tsProto, commonProto, fileProto},
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		t.Fatalf("protodesc.NewFiles: %v", err)
	}
	desc, err := files.FindDescriptorByName("atlantis.test.v1.TestFilter")
	if err != nil {
		t.Fatalf("FindDescriptorByName: %v", err)
	}
	return desc.(protoreflect.MessageDescriptor)
}

func optMsg(num int, name, typeName string) *descriptorpb.FieldDescriptorProto {
	t := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	l := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(int32(num)),
		Type:     &t,
		Label:    &l,
		TypeName: proto.String(typeName),
	}
}

func repMsg(num int, name, typeName string) *descriptorpb.FieldDescriptorProto {
	t := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	l := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	return &descriptorpb.FieldDescriptorProto{
		Name:     proto.String(name),
		Number:   proto.Int32(int32(num)),
		Type:     &t,
		Label:    &l,
		TypeName: proto.String(typeName),
	}
}

// testSpec returns a FilterSpec matching testFilterDescriptor's fields.
func testSpec() FilterSpec {
	return FilterSpec{
		EntityID:  "test.TestEntity",
		TableName: "test_entity",
		Fields: map[string]FieldSpec{
			"id":         {Column: "id", Kind: PredicateInt64},
			"name":       {Column: "name", Kind: PredicateString},
			"age":        {Column: "age", Kind: PredicateInt32},
			"active":     {Column: "active", Kind: PredicateBool},
			"created_at": {Column: "created_at", Kind: PredicateTimestamp},
			"raw":        {Column: "raw", Kind: PredicateBytes},
			"price":      {Column: "price", Kind: PredicateNumeric},
		},
	}
}

// newFilter constructs an empty TestFilter dynamic message.
func newFilter(t *testing.T) *dynamicpb.Message {
	t.Helper()
	return dynamicpb.NewMessage(testFilterDescriptor(t))
}

// setPredicate assigns a hand-built predicate onto a filter field.
// `predProto` is a real proto message we marshal+unmarshal into the
// dynamic predicate slot. This is the same path the gRPC dispatcher uses
// when the filter arrives over the wire.
func setPredicate(t *testing.T, filter *dynamicpb.Message, field string, predProto proto.Message) {
	t.Helper()
	fd := filter.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil {
		t.Fatalf("filter has no field %q", field)
	}
	// Build a dynamic message of the right predicate type, then unmarshal
	// the real predicate's bytes into it.
	dynPred := dynamicpb.NewMessage(fd.Message())
	bytes, err := proto.Marshal(predProto)
	if err != nil {
		t.Fatalf("marshal predicate: %v", err)
	}
	if err := proto.Unmarshal(bytes, dynPred); err != nil {
		t.Fatalf("unmarshal predicate: %v", err)
	}
	filter.Set(fd, protoreflect.ValueOfMessage(dynPred.ProtoReflect()))
}

func setComposite(t *testing.T, filter *dynamicpb.Message, field string, children []*dynamicpb.Message) {
	t.Helper()
	fd := filter.Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil {
		t.Fatalf("filter has no field %q", field)
	}
	if fd.IsList() {
		list := filter.Mutable(fd).List()
		for _, c := range children {
			list.Append(protoreflect.ValueOfMessage(c.ProtoReflect()))
		}
	} else {
		if len(children) != 1 {
			t.Fatalf("singular composite field wants 1 child, got %d", len(children))
		}
		filter.Set(fd, protoreflect.ValueOfMessage(children[0].ProtoReflect()))
	}
}

// translate is a thin wrapper for tests.
func translate(t *testing.T, f *dynamicpb.Message, extra ...string) (string, []any) {
	t.Helper()
	sql, args, _, err := TranslateFilter(testSpec(), f.ProtoReflect(), 1, extra...)
	if err != nil {
		t.Fatalf("TranslateFilter: %v", err)
	}
	return sql, args
}

func translateErr(t *testing.T, f *dynamicpb.Message) error {
	t.Helper()
	_, _, _, err := TranslateFilter(testSpec(), f.ProtoReflect(), 1)
	return err
}

// ----------------------------------------------------------------------------
// Unit tests — every predicate type × every arm

func TestStringPredicate_AllArms(t *testing.T) {
	cases := []struct {
		name    string
		pred    *commonv1.StringPredicate
		wantSQL string
		wantArg any
	}{
		{
			"eq",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Eq{Eq: "alice"}},
			"(name = $1)", "alice",
		},
		{
			"neq",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Neq{Neq: "alice"}},
			"(name <> $1)", "alice",
		},
		{
			"prefix",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Prefix{Prefix: "ali"}},
			"(name LIKE $1)", "ali%",
		},
		{
			"suffix",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Suffix{Suffix: "ce"}},
			"(name LIKE $1)", "%ce",
		},
		{
			"contains",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Contains{Contains: "lic"}},
			"(name LIKE $1)", "%lic%",
		},
		{
			"ilike",
			&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Ilike{Ilike: "ALI"}},
			"(name ILIKE $1)", "%ALI%",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFilter(t)
			setPredicate(t, f, "name", tc.pred)
			sql, args := translate(t, f)
			if sql != tc.wantSQL {
				t.Errorf("sql = %q, want %q", sql, tc.wantSQL)
			}
			if len(args) != 1 || args[0] != tc.wantArg {
				t.Errorf("args = %v, want [%v]", args, tc.wantArg)
			}
		})
	}
}

func TestStringPredicate_LikeEscaping(t *testing.T) {
	f := newFilter(t)
	// Caller smuggling a `%` should hit a literal `\%` in the bound value,
	// not a wildcard.
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Prefix{Prefix: "100%"},
	})
	_, args := translate(t, f)
	if args[0] != `100\%%` {
		t.Errorf("escape: got %q, want %q", args[0], `100\%%`)
	}
}

func TestStringPredicate_InList(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_In{In: &commonv1.StringList{Values: []string{"a", "b", "c"}}},
	})
	sql, args := translate(t, f)
	if sql != "(name IN ($1, $2, $3))" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 3 {
		t.Errorf("args len = %d, want 3", len(args))
	}
}

func TestStringPredicate_NullArms(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_IsNull{IsNull: true},
	})
	sql, args := translate(t, f)
	if sql != "(name IS NULL)" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("is_null should bind no args, got %v", args)
	}

	f2 := newFilter(t)
	setPredicate(t, f2, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_IsNotNull{IsNotNull: true},
	})
	sql2, _ := translate(t, f2)
	if sql2 != "(name IS NOT NULL)" {
		t.Errorf("sql2 = %q", sql2)
	}
}

func TestInt64Predicate_AllArms(t *testing.T) {
	cases := []struct {
		name    string
		pred    *commonv1.Int64Predicate
		wantSQL string
	}{
		{"eq", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Eq{Eq: 7}}, "(id = $1)"},
		{"neq", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Neq{Neq: 7}}, "(id <> $1)"},
		{"lt", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Lt{Lt: 7}}, "(id < $1)"},
		{"lte", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Lte{Lte: 7}}, "(id <= $1)"},
		{"gt", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Gt{Gt: 7}}, "(id > $1)"},
		{"gte", &commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Gte{Gte: 7}}, "(id >= $1)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFilter(t)
			setPredicate(t, f, "id", tc.pred)
			sql, args := translate(t, f)
			if sql != tc.wantSQL {
				t.Errorf("sql = %q", sql)
			}
			if got, want := args[0].(int64), int64(7); got != want {
				t.Errorf("arg = %v, want %v", got, want)
			}
		})
	}
}

func TestInt64Predicate_Between(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "id", &commonv1.Int64Predicate{
		Op: &commonv1.Int64Predicate_Between{Between: &commonv1.Int64Range{Lo: 10, Hi: 20}},
	})
	sql, args := translate(t, f)
	if sql != "(id BETWEEN $1 AND $2)" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 2 {
		t.Errorf("args len = %d", len(args))
	}
}

func TestInt64Predicate_BetweenInverted(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "id", &commonv1.Int64Predicate{
		Op: &commonv1.Int64Predicate_Between{Between: &commonv1.Int64Range{Lo: 20, Hi: 10}},
	})
	if err := translateErr(t, f); err == nil {
		t.Errorf("expected error for inverted range")
	}
}

func TestBoolPredicate(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "active", &commonv1.BoolPredicate{
		Op: &commonv1.BoolPredicate_Eq{Eq: true},
	})
	sql, args := translate(t, f)
	if sql != "(active = $1)" {
		t.Errorf("sql = %q", sql)
	}
	if args[0].(bool) != true {
		t.Errorf("arg = %v", args[0])
	}
}

func TestTimestampPredicate(t *testing.T) {
	now := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	f := newFilter(t)
	setPredicate(t, f, "created_at", &commonv1.TimestampPredicate{
		Op: &commonv1.TimestampPredicate_Gte{Gte: timestamppb.New(now)},
	})
	sql, args := translate(t, f)
	if sql != "(created_at >= $1)" {
		t.Errorf("sql = %q", sql)
	}
	got, ok := args[0].(time.Time)
	if !ok || !got.Equal(now) {
		t.Errorf("arg = %v, want %v", args[0], now)
	}
}

func TestBytesPredicate(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "raw", &commonv1.BytesPredicate{
		Op: &commonv1.BytesPredicate_Eq{Eq: []byte("hi")},
	})
	sql, args := translate(t, f)
	if sql != "(raw = $1)" {
		t.Errorf("sql = %q", sql)
	}
	if string(args[0].([]byte)) != "hi" {
		t.Errorf("arg = %v", args[0])
	}
}

func TestNumericPredicate(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "price", &commonv1.NumericPredicate{
		Op: &commonv1.NumericPredicate_Gte{Gte: "19.99"},
	})
	sql, args := translate(t, f)
	if sql != "(price::numeric >= $1::numeric)" {
		t.Errorf("sql = %q", sql)
	}
	if args[0].(string) != "19.99" {
		t.Errorf("arg = %v", args[0])
	}
}

// ----------------------------------------------------------------------------
// Composite arms

func TestComposite_AND(t *testing.T) {
	child1 := newFilter(t)
	setPredicate(t, child1, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "a"},
	})
	child2 := newFilter(t)
	setPredicate(t, child2, "age", &commonv1.Int32Predicate{
		Op: &commonv1.Int32Predicate_Gte{Gte: 18},
	})
	root := newFilter(t)
	setComposite(t, root, "and", []*dynamicpb.Message{child1, child2})
	sql, args := translate(t, root)
	// Two children → "(name = $1) AND (age >= $2)" wrapped in the root paren.
	if !strings.Contains(sql, "name = $1") || !strings.Contains(sql, "age >= $2") || !strings.Contains(sql, "AND") {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 2 {
		t.Errorf("args = %v", args)
	}
}

func TestComposite_OR(t *testing.T) {
	child1 := newFilter(t)
	setPredicate(t, child1, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "a"},
	})
	child2 := newFilter(t)
	setPredicate(t, child2, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "b"},
	})
	root := newFilter(t)
	setComposite(t, root, "or", []*dynamicpb.Message{child1, child2})
	sql, _ := translate(t, root)
	if !strings.Contains(sql, "OR") {
		t.Errorf("expected OR in %q", sql)
	}
}

func TestComposite_NOT(t *testing.T) {
	child := newFilter(t)
	setPredicate(t, child, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "a"},
	})
	root := newFilter(t)
	setComposite(t, root, "not", []*dynamicpb.Message{child})
	sql, _ := translate(t, root)
	if !strings.Contains(sql, "NOT") {
		t.Errorf("expected NOT in %q", sql)
	}
}

func TestComposite_FlattenSingle(t *testing.T) {
	// R3: and:[F] → F (no wrapping AND).
	child := newFilter(t)
	setPredicate(t, child, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "x"},
	})
	root := newFilter(t)
	setComposite(t, root, "and", []*dynamicpb.Message{child})
	sql, _ := translate(t, root)
	// Expect a single predicate without an explicit AND between any clauses
	// at the root level.
	if strings.Count(sql, " AND ") != 0 {
		t.Errorf("R3 flatten failed; sql = %q", sql)
	}
}

// ----------------------------------------------------------------------------
// Safety: depth cap, in list cap, string length cap

func TestSafety_DepthCap(t *testing.T) {
	// Nest deeper than MaxFilterDepth using `not`.
	leaf := newFilter(t)
	setPredicate(t, leaf, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "x"},
	})
	cur := leaf
	for i := 0; i < MaxFilterDepth+2; i++ {
		parent := newFilter(t)
		setComposite(t, parent, "not", []*dynamicpb.Message{cur})
		cur = parent
	}
	if err := translateErr(t, cur); err == nil {
		t.Errorf("expected depth-cap error")
	}
}

func TestSafety_InListCap(t *testing.T) {
	vals := make([]string, MaxInListSize+1)
	for i := range vals {
		vals[i] = "x"
	}
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_In{In: &commonv1.StringList{Values: vals}},
	})
	if err := translateErr(t, f); err == nil {
		t.Errorf("expected in-list-cap error")
	}
}

func TestSafety_StringLengthCap(t *testing.T) {
	big := strings.Repeat("a", MaxStringLen+1)
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: big},
	})
	if err := translateErr(t, f); err == nil {
		t.Errorf("expected string-cap error")
	}
}

// ----------------------------------------------------------------------------
// Edge cases

func TestEmpty_FilterReturnsEmptyFragment(t *testing.T) {
	f := newFilter(t)
	sql, args := translate(t, f)
	if sql != "" {
		t.Errorf("expected empty sql, got %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("expected no args, got %v", args)
	}
}

func TestEmpty_PredicateOneofUnset(t *testing.T) {
	// A StringPredicate with no oneof arm set — translator should drop it.
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{})
	sql, _ := translate(t, f)
	if sql != "" {
		t.Errorf("expected empty sql, got %q", sql)
	}
}

func TestExtraPredicates_AppendedWithAND(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "alice"},
	})
	sql, args := translate(t, f, "consumer_id = 'caller-A'")
	if !strings.Contains(sql, "name = $1") || !strings.Contains(sql, "consumer_id = 'caller-A'") {
		t.Errorf("sql missing required parts: %q", sql)
	}
	if !strings.Contains(sql, " AND ") {
		t.Errorf("extra predicate should be AND-joined: %q", sql)
	}
	if len(args) != 1 {
		t.Errorf("extra predicate carries its own placeholders; args = %v", args)
	}
}

func TestExtraPredicates_OnlyExtra_NoUserFilter(t *testing.T) {
	// User filter empty → just the extra predicates wrapped.
	sql, args, _, err := TranslateFilter(testSpec(), nil, 1, "deleted_at IS NULL")
	if err != nil {
		t.Fatalf("TranslateFilter: %v", err)
	}
	if sql != "(deleted_at IS NULL)" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 0 {
		t.Errorf("args = %v", args)
	}
}

func TestPlaceholderNumbering_StartsAtN(t *testing.T) {
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "alice"},
	})
	sql, args, consumed, err := TranslateFilter(testSpec(), f.ProtoReflect(), 5)
	if err != nil {
		t.Fatalf("TranslateFilter: %v", err)
	}
	if sql != "(name = $5)" {
		t.Errorf("sql = %q, expected $5", sql)
	}
	if consumed != 1 {
		t.Errorf("consumed = %d, want 1", consumed)
	}
	if len(args) != 1 {
		t.Errorf("args = %v", args)
	}
}

func TestCanonicalOrder_FieldsSortedByNumber(t *testing.T) {
	// Set fields in reverse order; translator should still emit them in
	// proto-field-number order (id=1, name=2, age=3 → SQL has id first).
	f := newFilter(t)
	setPredicate(t, f, "age", &commonv1.Int32Predicate{
		Op: &commonv1.Int32Predicate_Eq{Eq: 30},
	})
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "a"},
	})
	setPredicate(t, f, "id", &commonv1.Int64Predicate{
		Op: &commonv1.Int64Predicate_Eq{Eq: 1},
	})
	sql, _ := translate(t, f)
	// Find positions; id must come before name must come before age.
	pi := strings.Index(sql, "id ")
	pn := strings.Index(sql, "name ")
	pa := strings.Index(sql, "age ")
	if !(pi < pn && pn < pa) {
		t.Errorf("expected id < name < age order; sql = %q", sql)
	}
}

func TestUnknownFilterField(t *testing.T) {
	// Pass a FilterSpec missing one of the fields the message has set.
	f := newFilter(t)
	setPredicate(t, f, "name", &commonv1.StringPredicate{
		Op: &commonv1.StringPredicate_Eq{Eq: "x"},
	})
	spec := testSpec()
	delete(spec.Fields, "name")
	_, _, _, err := TranslateFilter(spec, f.ProtoReflect(), 1)
	if err == nil {
		t.Errorf("expected error for unknown filter field")
	}
}

// ----------------------------------------------------------------------------
// Fuzz: random filter bytes → translate → assert safety invariants

func FuzzTranslator_NoInjection(f *testing.F) {
	// Seed with a few realistic predicates.
	for _, seed := range [][]byte{
		mustMarshal(&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Eq{Eq: "alice"}}),
		mustMarshal(&commonv1.StringPredicate{Op: &commonv1.StringPredicate_Contains{Contains: "x' OR 1=1--"}}),
		mustMarshal(&commonv1.Int64Predicate{Op: &commonv1.Int64Predicate_Eq{Eq: 42}}),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		filter := dynamicpbNewFilter(t)
		// Try to interpret `data` as bytes for the `name` slot. Skip if
		// it doesn't decode as a StringPredicate.
		dyn := dynamicpb.NewMessage(filter.Descriptor().Fields().ByName("name").Message())
		if err := proto.Unmarshal(data, dyn); err != nil {
			return
		}
		filter.Set(filter.Descriptor().Fields().ByName("name"), protoreflect.ValueOfMessage(dyn.ProtoReflect()))
		sql, args, _, err := TranslateFilter(testSpec(), filter.ProtoReflect(), 1)
		if err != nil {
			return // rejection is the safety contract; rejected inputs are fine
		}
		// Safety invariants the fuzzer asserts:
		// 1. Every $N in sql has a corresponding args entry.
		placeholders := strings.Count(sql, "$")
		if placeholders != len(args) {
			t.Errorf("placeholder count %d ≠ args count %d; sql=%q args=%v", placeholders, len(args), sql, args)
		}
		// 2. No semicolon (statement injection).
		if strings.ContainsRune(sql, ';') {
			t.Errorf("sql contains semicolon: %q", sql)
		}
		// 3. SQL never contains a raw string value from args (which would
		//    mean the value was interpolated rather than bound). Probe
		//    each string arg as a substring.
		for _, a := range args {
			if s, ok := a.(string); ok && len(s) >= 4 && strings.Contains(sql, s) {
				t.Errorf("arg %q appears uninterpolated in sql %q", s, sql)
			}
		}
	})
}

// dynamicpbNewFilter is the fuzz-time equivalent of newFilter without the
// *testing.T plumbing for fuzz reuse.
func dynamicpbNewFilter(t *testing.T) *dynamicpb.Message {
	t.Helper()
	return dynamicpb.NewMessage(testFilterDescriptor(t))
}

func mustMarshal(m proto.Message) []byte {
	b, _ := proto.Marshal(m)
	return b
}
