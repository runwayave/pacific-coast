package entity

import (
	"fmt"

	"github.com/rachitkumar205/atlantis/internal/dsl"
	"github.com/rachitkumar205/atlantis/internal/schema"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

// buildProtoDescriptors constructs protoreflect descriptors for an entity
// at runtime using descriptorpb. The resulting message descriptors use
// the same field numbers the codegen assigns (Field.ProtoNumber), so
// callers sending compiled proto messages produce identical wire bytes.
//
// Returns the file descriptor containing all messages and services for this entity.
func buildProtoDescriptors(e *dsl.Entity) (protoreflect.FileDescriptor, error) {
	ns := goNamespace(e.Namespace)
	pkg := fmt.Sprintf("atlantis.%s.v1", ns)
	fileName := fmt.Sprintf("atlantis/%s/v1/%s_dynamic.proto", ns, schema.SnakeCase(e.Name))

	file := &descriptorpb.FileDescriptorProto{
		Name:    strPtr(fileName),
		Package: strPtr(pkg),
		Syntax:  strPtr("proto3"),
	}

	// Check if we need the Timestamp import.
	needsTimestamp := false
	for _, f := range e.Fields {
		if f.Type.Name == "timestamptz" || f.Type.Name == "date" {
			needsTimestamp = true
			break
		}
	}

	// Entity message.
	entityMsg := buildEntityMessage(e)
	file.MessageType = append(file.MessageType, entityMsg)

	// PK columns for request shells.
	pkCols := schema.PKColumns(e)
	composite := len(pkCols) > 1

	// Composite PK wrapper message.
	if composite {
		pkMsg := buildPKMessage(e, pkCols)
		file.MessageType = append(file.MessageType, pkMsg)
	}

	// Request/Response messages.
	file.MessageType = append(file.MessageType,
		buildGetRequest(e, pkCols),
		buildGetResponse(e),
		buildCreateRequest(e),
		buildCreateResponse(e),
		buildUpdateRequest(e),
		buildUpdateResponse(e),
		buildDeleteRequest(e, pkCols),
		buildDeleteResponse(e),
		buildBatchGetRequest(e, pkCols, composite),
		buildBatchGetResponse(e),
		buildQueryRequest(e),
		buildQueryResponse(e),
	)

	// Filter message.
	filterMsg := buildFilterMessage(e)
	file.MessageType = append(file.MessageType, filterMsg)

	// Service definition.
	svc := buildServiceDescriptor(e)
	file.Service = append(file.Service, svc)

	// Declare dependencies. The Timestamp well-known type and the
	// common predicates proto are needed if this entity uses timestamps
	// or has filterable fields (the filter message references predicate
	// message types from atlantis.common.v1).
	if needsTimestamp {
		file.Dependency = append(file.Dependency, "google/protobuf/timestamp.proto")
	}
	// The filter message always references predicates from
	// atlantis/common/v1/predicates.proto (unless no fields are
	// filterable, which is degenerate). Add the dependency
	// unconditionally — protodesc tolerates unused deps.
	file.Dependency = append(file.Dependency, "atlantis/common/v1/predicates.proto")

	// Build a resolver that chains our custom built files with the
	// global proto registry (which holds the compiled Timestamp,
	// predicates, etc. from init()-time registration).
	resolver := &fileResolver{
		files:  make(map[string]protoreflect.FileDescriptor),
		global: protoregistry.GlobalFiles,
	}

	fd, err := protodesc.NewFile(file, resolver)
	if err != nil {
		return nil, fmt.Errorf("building file descriptor for %s: %w", e.ID(), err)
	}

	// Sanity check: the entity message and filter message must be present.
	entityDesc := fd.Messages().ByName(protoreflect.Name(e.Name))
	if entityDesc == nil {
		return nil, fmt.Errorf("entity message %s not found in built descriptor", e.Name)
	}
	filterName := protoreflect.Name(e.Name + "Filter")
	filterDescr := fd.Messages().ByName(filterName)
	if filterDescr == nil {
		return nil, fmt.Errorf("filter message %s not found in built descriptor", filterName)
	}

	return fd, nil
}

func buildEntityMessage(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name),
	}

	for _, f := range e.Fields {
		field := dslFieldToProtoField(&f)
		msg.Field = append(msg.Field, field)
	}

	// Reserved numbers for retired proto fields.
	if len(e.RetiredProtoNumbers) > 0 {
		for _, n := range e.RetiredProtoNumbers {
			n32 := int32(n)
			msg.ReservedRange = append(msg.ReservedRange, &descriptorpb.DescriptorProto_ReservedRange{
				Start: &n32,
				End:   int32Ptr(n32 + 1), // end is exclusive
			})
		}
	}

	return msg
}

// dslFieldToProtoField converts a DSL field to a proto FieldDescriptorProto
// using the same type mapping as coltype.ProtoType.
func dslFieldToProtoField(f *dsl.Field) *descriptorpb.FieldDescriptorProto {
	num := int32(f.ProtoNumber)
	fd := &descriptorpb.FieldDescriptorProto{
		Name:   strPtr(f.Name),
		Number: &num,
	}

	applyProtoFieldType(fd, f.Type)
	// Nullable scalars are proto3-optional for presence tracking; repeated
	// fields (array/vector) never take proto3-optional. protodesc.NewFile
	// materializes the synthetic oneof from Proto3Optional.
	if !f.Type.Array && f.Type.Name != "vector" && schema.IsEffectivelyNullable(f) {
		fd.Proto3Optional = boolPtr(true)
	}
	return fd
}

// applyProtoFieldType sets a field descriptor's Type and Label from a DSL
// field type: vector(N) and []T are LABEL_REPEATED (repeated float /
// repeated <elem>), every other type is a LABEL_OPTIONAL scalar. This is
// the single place that decides a field's proto cardinality, shared by the
// entity and custom-query descriptor builders so the two can't diverge.
// They did once: the custom-query builder hard-coded LABEL_OPTIONAL and
// left vector inputs as a scalar float, so the client's packed 768-float
// payload hit a wire-type mismatch (skipped → 0) and the runtime
// dispatcher panicked ("cannot convert float32 to list") once it tried to
// read the field as a list.
func applyProtoFieldType(fd *descriptorpb.FieldDescriptorProto, t dsl.FieldType) {
	if t.Array {
		rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		fd.Label = &rep
		if t.Elem != nil {
			setProtoType(fd, *t.Elem)
		} else {
			// bare array with no elem info — default to string
			typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
			fd.Type = &typ
		}
		return
	}
	if t.Name == "vector" {
		rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
		fd.Label = &rep
		typ := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
		fd.Type = &typ
		return
	}
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	fd.Label = &opt
	setProtoType(fd, t)
}

func setProtoType(fd *descriptorpb.FieldDescriptorProto, t dsl.FieldType) {
	switch t.Name {
	case "smallint", "int":
		typ := descriptorpb.FieldDescriptorProto_TYPE_INT32
		fd.Type = &typ
	case "bigint":
		typ := descriptorpb.FieldDescriptorProto_TYPE_INT64
		fd.Type = &typ
	case "text", "varchar", "citext", "uuid", "numeric":
		typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
		fd.Type = &typ
	case "boolean":
		typ := descriptorpb.FieldDescriptorProto_TYPE_BOOL
		fd.Type = &typ
	case "timestamptz", "date":
		typ := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		fd.Type = &typ
		fd.TypeName = strPtr(".google.protobuf.Timestamp")
	case "bytea", "jsonb":
		typ := descriptorpb.FieldDescriptorProto_TYPE_BYTES
		fd.Type = &typ
	case "interval":
		// Rendered as string (Postgres INTERVAL has no native proto type
		// and the codegen historically maps it to string).
		typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
		fd.Type = &typ
	case "vector":
		typ := descriptorpb.FieldDescriptorProto_TYPE_FLOAT
		fd.Type = &typ
	default:
		typ := descriptorpb.FieldDescriptorProto_TYPE_STRING
		fd.Type = &typ
	}
}

func buildPKMessage(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name + "PK"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildGetRequest(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Get" + e.Name + "Request"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildGetResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Get"+e.Name+"Response", e)
}

func buildCreateRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest("Create"+e.Name+"Request", e)
}

func buildCreateResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Create"+e.Name+"Response", e)
}

func buildUpdateRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest("Update"+e.Name+"Request", e)
}

func buildUpdateResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityResponse("Update"+e.Name+"Response", e)
}

func buildDeleteRequest(e *dsl.Entity, pkCols []*dsl.Field) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Delete" + e.Name + "Request"),
	}
	for i, f := range pkCols {
		num := int32(i + 1)
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(f.Name),
			Number: &num,
		}
		label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		fd.Label = &label
		setProtoType(fd, f.Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildDeleteResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{
		Name: strPtr("Delete" + e.Name + "Response"),
	}
}

func buildBatchGetRequest(e *dsl.Entity, pkCols []*dsl.Field, composite bool) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("BatchGet" + e.Name + "Request"),
	}
	one := int32(1)
	label := descriptorpb.FieldDescriptorProto_LABEL_REPEATED

	if composite {
		// repeated <Entity>PK ids = 1;
		ns := goNamespace(e.Namespace)
		typeName := fmt.Sprintf(".atlantis.%s.v1.%sPK", ns, e.Name)
		typ := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr("ids"),
			Number:   &one,
			Label:    &label,
			Type:     &typ,
			TypeName: strPtr(typeName),
		})
	} else {
		// repeated <pk_type> <pk_name>s = 1;
		fd := &descriptorpb.FieldDescriptorProto{
			Name:   strPtr(pkCols[0].Name + "s"),
			Number: &one,
			Label:  &label,
		}
		setProtoType(fd, pkCols[0].Type)
		msg.Field = append(msg.Field, fd)
	}
	return msg
}

func buildBatchGetResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapRepeatedEntityResponse("BatchGet"+e.Name+"Response", e)
}

func buildQueryRequest(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Query" + e.Name + "Request"),
	}
	ns := goNamespace(e.Namespace)
	filterTypeName := fmt.Sprintf(".atlantis.%s.v1.%sFilter", ns, e.Name)

	one := int32(1)
	three := int32(3)
	four := int32(4)
	seven := int32(7)

	filterType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	int32Type := descriptorpb.FieldDescriptorProto_TYPE_INT32
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	boolType := descriptorpb.FieldDescriptorProto_TYPE_BOOL
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("filter"),
			Number:   &one,
			Label:    &optLabel,
			Type:     &filterType,
			TypeName: strPtr(filterTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("limit"),
			Number: &three,
			Label:  &optLabel,
			Type:   &int32Type,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("page_token"),
			Number: &four,
			Label:  &optLabel,
			Type:   &stringType,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("cache_skip"),
			Number: &seven,
			Label:  &optLabel,
			Type:   &boolType,
		},
	)
	return msg
}

func buildQueryResponse(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr("Query" + e.Name + "Response"),
	}
	ns := goNamespace(e.Namespace)
	entityTypeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)

	one := int32(1)
	two := int32(2)
	three := int32(3)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	stringType := descriptorpb.FieldDescriptorProto_TYPE_STRING
	int64Type := descriptorpb.FieldDescriptorProto_TYPE_INT64
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("entities"),
			Number:   &one,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(entityTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:   strPtr("next_page_token"),
			Number: &two,
			Label:  &optLabel,
			Type:   &stringType,
		},
		&descriptorpb.FieldDescriptorProto{
			Name:           strPtr("total_estimate"),
			Number:         &three,
			Label:          &optLabel,
			Type:           &int64Type,
			Proto3Optional: boolPtr(true),
		},
	)
	return msg
}

// buildFilterMessage creates the <Entity>Filter message. One field per
// filterable column, using the predicate message type naming convention.
func buildFilterMessage(e *dsl.Entity) *descriptorpb.DescriptorProto {
	msg := &descriptorpb.DescriptorProto{
		Name: strPtr(e.Name + "Filter"),
	}

	// Filter fields use field numbers starting at 1, in proto number order
	// of the entity fields. Only filterable fields get a slot.
	num := int32(1)
	for _, f := range e.Fields {
		predMsg, ok := predicateMessageForField(f.Type)
		if !ok {
			continue
		}
		msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
		optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
		typeName := fmt.Sprintf(".atlantis.common.v1.%s", predMsg)
		n := num
		msg.Field = append(msg.Field, &descriptorpb.FieldDescriptorProto{
			Name:     strPtr(f.Name),
			Number:   &n,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(typeName),
		})
		num++
	}

	// Composition arms: and, or, not (standard filter composition fields).
	ns := goNamespace(e.Namespace)
	selfTypeName := fmt.Sprintf(".atlantis.%s.v1.%sFilter", ns, e.Name)
	andNum := int32(100)
	orNum := int32(101)
	notNum := int32(102)
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE

	msg.Field = append(msg.Field,
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("and"),
			Number:   &andNum,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("or"),
			Number:   &orNum,
			Label:    &repLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
		&descriptorpb.FieldDescriptorProto{
			Name:     strPtr("not"),
			Number:   &notNum,
			Label:    &optLabel,
			Type:     &msgType,
			TypeName: strPtr(selfTypeName),
		},
	)

	return msg
}

func buildServiceDescriptor(e *dsl.Entity) *descriptorpb.ServiceDescriptorProto {
	ns := goNamespace(e.Namespace)
	pkg := fmt.Sprintf(".atlantis.%s.v1", ns)

	svc := &descriptorpb.ServiceDescriptorProto{
		Name: strPtr(e.Name + "Service"),
	}

	// Standard CRUD methods.
	methods := []struct {
		name   string
		input  string
		output string
	}{
		{"Get" + e.Name, "Get" + e.Name + "Request", "Get" + e.Name + "Response"},
		{"Create" + e.Name, "Create" + e.Name + "Request", "Create" + e.Name + "Response"},
		{"Update" + e.Name, "Update" + e.Name + "Request", "Update" + e.Name + "Response"},
		{"Delete" + e.Name, "Delete" + e.Name + "Request", "Delete" + e.Name + "Response"},
		{"BatchGet" + e.Name, "BatchGet" + e.Name + "Request", "BatchGet" + e.Name + "Response"},
		{"Query" + e.Name, "Query" + e.Name + "Request", "Query" + e.Name + "Response"},
	}

	for _, m := range methods {
		svc.Method = append(svc.Method, &descriptorpb.MethodDescriptorProto{
			Name:       strPtr(m.name),
			InputType:  strPtr(pkg + "." + m.input),
			OutputType: strPtr(pkg + "." + m.output),
		})
	}

	return svc
}

// predicateMessageForField maps a DSL field type to the predicate proto
// message name. Mirrors codegen/query_emit.go predicateMessageForField.
func predicateMessageForField(t dsl.FieldType) (string, bool) {
	if t.Array {
		return "", false
	}
	switch t.Name {
	case "text", "varchar", "citext", "uuid":
		return "StringPredicate", true
	case "numeric":
		return "NumericPredicate", true
	case "int", "smallint":
		return "Int32Predicate", true
	case "bigint":
		return "Int64Predicate", true
	case "boolean":
		return "BoolPredicate", true
	case "timestamptz", "date":
		return "TimestampPredicate", true
	case "jsonb", "bytea":
		return "BytesPredicate", true
	}
	return "", false
}

// wrapEntityRequest creates a message with a single entity field at number 1.
func wrapEntityRequest(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	ns := goNamespace(e.Namespace)
	typeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)
	one := int32(1)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	optLabel := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	return &descriptorpb.DescriptorProto{
		Name: strPtr(name),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     strPtr("entity"),
				Number:   &one,
				Label:    &optLabel,
				Type:     &msgType,
				TypeName: strPtr(typeName),
			},
		},
	}
}

// wrapEntityResponse creates a message with a single entity field at number 1.
func wrapEntityResponse(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	return wrapEntityRequest(name, e)
}

// wrapRepeatedEntityResponse creates a message with a repeated entity field at number 1.
func wrapRepeatedEntityResponse(name string, e *dsl.Entity) *descriptorpb.DescriptorProto {
	ns := goNamespace(e.Namespace)
	typeName := fmt.Sprintf(".atlantis.%s.v1.%s", ns, e.Name)
	one := int32(1)
	msgType := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	repLabel := descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	return &descriptorpb.DescriptorProto{
		Name: strPtr(name),
		Field: []*descriptorpb.FieldDescriptorProto{
			{
				Name:     strPtr("entities"),
				Number:   &one,
				Label:    &repLabel,
				Type:     &msgType,
				TypeName: strPtr(typeName),
			},
		},
	}
}

// goNamespace mirrors codegen/proto.go — maps "vendor" to "vendorpkg".
func goNamespace(ns string) string {
	if ns == "vendor" {
		return "vendorpkg"
	}
	return ns
}

// fileResolver implements protodesc.Resolver for dependency lookup.
// It chains a local map of built files with the global proto registry
// so that compiled well-known types (Timestamp) and the predicates
// proto are automatically available.
type fileResolver struct {
	files  map[string]protoreflect.FileDescriptor
	global *protoregistry.Files
}

func (r *fileResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if fd, ok := r.files[path]; ok {
		return fd, nil
	}
	if r.global != nil {
		return r.global.FindFileByPath(path)
	}
	return nil, fmt.Errorf("file not found: %s", path)
}

func (r *fileResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, fd := range r.files {
		if d := findInFile(fd, name); d != nil {
			return d, nil
		}
	}
	if r.global != nil {
		return r.global.FindDescriptorByName(name)
	}
	return nil, fmt.Errorf("descriptor not found: %s", name)
}

// findInFile searches a file descriptor for a named descriptor.
func findInFile(fd protoreflect.FileDescriptor, name protoreflect.FullName) protoreflect.Descriptor {
	msgs := fd.Messages()
	for i := range msgs.Len() {
		m := msgs.Get(i)
		if m.FullName() == name {
			return m
		}
	}
	svcs := fd.Services()
	for i := range svcs.Len() {
		s := svcs.Get(i)
		if s.FullName() == name {
			return s
		}
	}
	return nil
}

// Helper functions for creating pointers to proto primitive types.
func strPtr(s string) *string { return &s }
func int32Ptr(i int32) *int32 { return &i }
func boolPtr(b bool) *bool    { return &b }
