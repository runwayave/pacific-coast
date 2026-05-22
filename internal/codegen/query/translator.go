package query

import (
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// TranslateFilter converts a typed filter message into a parameterized SQL
// WHERE fragment and the bind args in $N order. It accepts any entity's
// XFilter via protoreflect — one implementation handles every entity.
//
// Inputs:
//   - spec:             codegen-supplied field metadata for the filter's entity.
//   - msg:              the filter message (nil or zero-value → no-op).
//   - placeholderStart: $N of the first arg this call will allocate.
//   - extraPredicates:  pre-built SQL fragments AND-joined onto the user filter
//     AFTER normalization (auth, partition, cursor predicates).
//     Each entry must already use placeholders that don't
//     collide with placeholderStart..placeholderStart+returned.
//
// Returns the WHERE fragment without a leading "WHERE", the bind args, and
// the number of placeholders consumed (so the caller can resume numbering
// for LIMIT / ORDER BY values).
func TranslateFilter(
	spec FilterSpec,
	msg protoreflect.Message,
	placeholderStart int,
	extraPredicates ...string,
) (sqlFragment string, args []any, consumed int, err error) {
	w := walker{
		spec:       spec,
		nextPH:     placeholderStart,
		startingPH: placeholderStart,
	}

	var userPart string
	if msg != nil && msg.IsValid() {
		userPart, err = w.translateFilter(msg, 0)
		if err != nil {
			return "", nil, 0, err
		}
	}

	parts := make([]string, 0, 1+len(extraPredicates))
	if userPart != "" {
		parts = append(parts, "("+userPart+")")
	}
	for _, ep := range extraPredicates {
		if ep == "" {
			continue
		}
		parts = append(parts, "("+ep+")")
	}

	return strings.Join(parts, " AND "), w.args, w.nextPH - w.startingPH, nil
}

// walker carries the placeholder counter + accumulated bind args through
// the recursive descent.
type walker struct {
	spec       FilterSpec
	nextPH     int
	startingPH int
	args       []any
}

// setField pairs a set field's descriptor with its value. Surfaced at
// package scope so sortByNumber can take a typed slice.
type setField struct {
	fd  protoreflect.FieldDescriptor
	val protoreflect.Value
}

// allocate reserves one placeholder and binds its arg.
func (w *walker) allocate(v any) string {
	ph := fmt.Sprintf("$%d", w.nextPH)
	w.nextPH++
	w.args = append(w.args, v)
	return ph
}

// translateFilter walks one XFilter message. Recursion depth is the number
// of and/or/not levels crossed; capped at MaxFilterDepth.
func (w *walker) translateFilter(m protoreflect.Message, depth int) (string, error) {
	if depth > MaxFilterDepth {
		return "", fmt.Errorf("%s: filter depth exceeds %d", w.spec.EntityID, MaxFilterDepth)
	}

	// Collect set fields, ordered by proto field number for canonical-form
	// stability.
	var set []setField
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		set = append(set, setField{fd, v})
		return true
	})
	// Range yields fields in undefined order; sort by number.
	sortByNumber(set)

	var parts []string
	for _, sf := range set {
		name := string(sf.fd.Name())
		switch name {
		case "and":
			frag, err := w.translateComposite(sf.val.List(), "AND", depth+1)
			if err != nil {
				return "", err
			}
			if frag != "" {
				parts = append(parts, frag)
			}
		case "or":
			frag, err := w.translateComposite(sf.val.List(), "OR", depth+1)
			if err != nil {
				return "", err
			}
			if frag != "" {
				parts = append(parts, frag)
			}
		case "not":
			inner, err := w.translateFilter(sf.val.Message(), depth+1)
			if err != nil {
				return "", err
			}
			if inner != "" {
				parts = append(parts, "NOT ("+inner+")")
			}
		default:
			fieldSpec, ok := w.spec.Fields[name]
			if !ok {
				return "", fmt.Errorf("%s: unknown filter field %q", w.spec.EntityID, name)
			}
			frag, err := w.translatePredicate(sf.val.Message(), fieldSpec)
			if err != nil {
				return "", fmt.Errorf("%s.%s: %w", w.spec.EntityID, name, err)
			}
			if frag != "" {
				parts = append(parts, frag)
			}
		}
	}

	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		return parts[0], nil
	}
	return strings.Join(parts, " AND "), nil
}

// translateComposite renders an `and` / `or` list of nested filters.
// Empty list yields "" (caller drops it). Single element unwraps to the
// inner fragment (composite flattening).
func (w *walker) translateComposite(list protoreflect.List, op string, depth int) (string, error) {
	if list.Len() == 0 {
		return "", nil
	}
	parts := make([]string, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		frag, err := w.translateFilter(list.Get(i).Message(), depth)
		if err != nil {
			return "", err
		}
		if frag != "" {
			parts = append(parts, "("+frag+")")
		}
	}
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 {
		// Single arm — drop the wrapping composite.
		return strings.TrimPrefix(strings.TrimSuffix(parts[0], ")"), "("), nil
	}
	return strings.Join(parts, " "+op+" "), nil
}

// translatePredicate dispatches to the per-kind helper based on FieldSpec.
func (w *walker) translatePredicate(pred protoreflect.Message, fs FieldSpec) (string, error) {
	switch fs.Kind {
	case PredicateString:
		return w.translateStringPredicate(pred, fs)
	case PredicateInt32:
		return w.translateIntPredicate(pred, fs, false)
	case PredicateInt64:
		return w.translateIntPredicate(pred, fs, true)
	case PredicateBool:
		return w.translateBoolPredicate(pred, fs)
	case PredicateTimestamp:
		return w.translateTimestampPredicate(pred, fs)
	case PredicateBytes:
		return w.translateBytesPredicate(pred, fs)
	case PredicateNumeric:
		return w.translateNumericPredicate(pred, fs)
	default:
		return "", fmt.Errorf("unsupported predicate kind %d for column %s", fs.Kind, fs.Column)
	}
}

// oneofArm finds the set arm of the predicate's `op` oneof and returns its
// field descriptor + value. Returns (nil, zero, false) if no arm is set
// (an empty predicate — caller drops it).
func oneofArm(m protoreflect.Message) (protoreflect.FieldDescriptor, protoreflect.Value, bool) {
	oneof := m.Descriptor().Oneofs().ByName("op")
	if oneof == nil {
		return nil, protoreflect.Value{}, false
	}
	fd := m.WhichOneof(oneof)
	if fd == nil {
		return nil, protoreflect.Value{}, false
	}
	return fd, m.Get(fd), true
}

// ----------------------------------------------------------------------------
// String predicate

func (w *walker) translateStringPredicate(m protoreflect.Message, fs FieldSpec) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s = %s", fs.Column, w.allocate(s)), nil
	case "neq":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s <> %s", fs.Column, w.allocate(s)), nil
	case "in":
		vals, err := stringListValues(val.Message())
		if err != nil {
			return "", err
		}
		return w.inFragment(fs.Column, vals, false)
	case "not_in":
		vals, err := stringListValues(val.Message())
		if err != nil {
			return "", err
		}
		return w.inFragment(fs.Column, vals, true)
	case "prefix":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s LIKE %s", fs.Column, w.allocate(escapeLike(s)+"%")), nil
	case "suffix":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s LIKE %s", fs.Column, w.allocate("%"+escapeLike(s))), nil
	case "contains":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s LIKE %s", fs.Column, w.allocate("%"+escapeLike(s)+"%")), nil
	case "ilike":
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s ILIKE %s", fs.Column, w.allocate("%"+escapeLike(s)+"%")), nil
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown string predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Int32 / Int64 predicate (one helper, parameterized by width)

func (w *walker) translateIntPredicate(m protoreflect.Message, fs FieldSpec, wide bool) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq", "neq", "lt", "lte", "gt", "gte":
		op := comparisonOp(arm)
		var bound any
		if wide {
			bound = val.Int()
		} else {
			bound = int32(val.Int())
		}
		return fmt.Sprintf("%s %s %s", fs.Column, op, w.allocate(bound)), nil
	case "in", "not_in":
		listMsg := val.Message()
		vals, err := intListValues(listMsg, wide)
		if err != nil {
			return "", err
		}
		return w.inFragment(fs.Column, vals, arm == "not_in")
	case "between":
		rng := val.Message()
		loF := rng.Descriptor().Fields().ByName("lo")
		hiF := rng.Descriptor().Fields().ByName("hi")
		lo := rng.Get(loF).Int()
		hi := rng.Get(hiF).Int()
		if lo > hi {
			return "", fmt.Errorf("range lo (%d) > hi (%d)", lo, hi)
		}
		var loVal, hiVal any
		if wide {
			loVal, hiVal = lo, hi
		} else {
			loVal, hiVal = int32(lo), int32(hi)
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", fs.Column, w.allocate(loVal), w.allocate(hiVal)), nil
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown int predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Bool predicate

func (w *walker) translateBoolPredicate(m protoreflect.Message, fs FieldSpec) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq":
		return fmt.Sprintf("%s = %s", fs.Column, w.allocate(val.Bool())), nil
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown bool predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Timestamp predicate

func (w *walker) translateTimestampPredicate(m protoreflect.Message, fs FieldSpec) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq", "neq", "lt", "lte", "gt", "gte":
		op := comparisonOp(arm)
		t, err := timestampFromMessage(val.Message())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s %s", fs.Column, op, w.allocate(t)), nil
	case "between":
		rng := val.Message()
		loF := rng.Descriptor().Fields().ByName("lo")
		hiF := rng.Descriptor().Fields().ByName("hi")
		lo, err := timestampFromMessage(rng.Get(loF).Message())
		if err != nil {
			return "", fmt.Errorf("range.lo: %w", err)
		}
		hi, err := timestampFromMessage(rng.Get(hiF).Message())
		if err != nil {
			return "", fmt.Errorf("range.hi: %w", err)
		}
		if lo.After(hi) {
			return "", fmt.Errorf("range lo > hi")
		}
		return fmt.Sprintf("%s BETWEEN %s AND %s", fs.Column, w.allocate(lo), w.allocate(hi)), nil
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown timestamp predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Bytes predicate

func (w *walker) translateBytesPredicate(m protoreflect.Message, fs FieldSpec) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq":
		return fmt.Sprintf("%s = %s", fs.Column, w.allocate(val.Bytes())), nil
	case "neq":
		return fmt.Sprintf("%s <> %s", fs.Column, w.allocate(val.Bytes())), nil
	case "in":
		vals, err := bytesListValues(val.Message())
		if err != nil {
			return "", err
		}
		return w.inFragment(fs.Column, vals, false)
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown bytes predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Numeric predicate (decimal-string-carried; cast to ::numeric for compares)

func (w *walker) translateNumericPredicate(m protoreflect.Message, fs FieldSpec) (string, error) {
	fd, val, ok := oneofArm(m)
	if !ok {
		return "", nil
	}
	arm := string(fd.Name())
	switch arm {
	case "eq", "neq", "lt", "lte", "gt", "gte":
		op := comparisonOp(arm)
		s, err := checkString(val.String())
		if err != nil {
			return "", err
		}
		// Cast both sides to numeric so PG compares as numbers, not strings.
		return fmt.Sprintf("%s::numeric %s %s::numeric", fs.Column, op, w.allocate(s)), nil
	case "in":
		vals, err := stringListValues(val.Message())
		if err != nil {
			return "", err
		}
		if len(vals) == 0 {
			return "", nil
		}
		phs := make([]string, len(vals))
		for i, v := range vals {
			phs[i] = w.allocate(v) + "::numeric"
		}
		return fmt.Sprintf("%s::numeric IN (%s)", fs.Column, strings.Join(phs, ", ")), nil
	case "is_null":
		return nullFragment(fs.Column, val.Bool(), false), nil
	case "is_not_null":
		return nullFragment(fs.Column, val.Bool(), true), nil
	default:
		return "", fmt.Errorf("unknown numeric predicate arm %q", arm)
	}
}

// ----------------------------------------------------------------------------
// Shared helpers

// comparisonOp maps a predicate arm name to a SQL operator.
func comparisonOp(arm string) string {
	switch arm {
	case "eq":
		return "="
	case "neq":
		return "<>"
	case "lt":
		return "<"
	case "lte":
		return "<="
	case "gt":
		return ">"
	case "gte":
		return ">="
	}
	return ""
}

// inFragment emits `col IN ($1, $2, ...)` or `col NOT IN (...)`. Empty list
// is a contradiction (IN empty) or tautology (NOT IN empty); we treat both
// as "no predicate contribution" since the canonicalizer drops empty lists.
// Direct empty arrivals (caller bug, no canonicalizer between) → drop.
func (w *walker) inFragment(col string, vals []any, negate bool) (string, error) {
	if len(vals) == 0 {
		return "", nil
	}
	if len(vals) > MaxInListSize {
		return "", fmt.Errorf("in/not_in list exceeds %d (got %d)", MaxInListSize, len(vals))
	}
	phs := make([]string, len(vals))
	for i, v := range vals {
		phs[i] = w.allocate(v)
	}
	op := "IN"
	if negate {
		op = "NOT IN"
	}
	return fmt.Sprintf("%s %s (%s)", col, op, strings.Join(phs, ", ")), nil
}

// nullFragment renders `col IS NULL` / `col IS NOT NULL`.
//
// The proto carries a `bool` on the is_null / is_not_null arm — the value is
// always true in practice (callers set the arm to opt in). We honor false as
// a no-op so a default-zero proto doesn't accidentally inject a predicate.
func nullFragment(col string, on bool, invert bool) string {
	if !on {
		return ""
	}
	if invert {
		return col + " IS NOT NULL"
	}
	return col + " IS NULL"
}

// checkString enforces the MaxStringLen cap.
func checkString(s string) (string, error) {
	if len(s) > MaxStringLen {
		return "", fmt.Errorf("string value exceeds %d bytes (got %d)", MaxStringLen, len(s))
	}
	return s, nil
}

// escapeLike escapes the PG LIKE metacharacters (`%`, `_`, `\`) so a caller
// can't smuggle a wildcard into a `prefix` / `suffix` / `contains` value.
// We use the default backslash escape character — PG's `LIKE` honors it
// unless `STANDARD_CONFORMING_STRINGS` is off (it's on in modern PG default).
func escapeLike(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\', '%', '_':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// stringListValues unpacks a StringList message's `values` field. Returns
// the raw strings AS []any so the caller can splice them into inFragment.
// Enforces R8 length cap on each element.
func stringListValues(m protoreflect.Message) ([]any, error) {
	fd := m.Descriptor().Fields().ByName("values")
	if fd == nil {
		return nil, fmt.Errorf("StringList missing `values` field")
	}
	list := m.Get(fd).List()
	out := make([]any, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		s := list.Get(i).String()
		if _, err := checkString(s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func intListValues(m protoreflect.Message, wide bool) ([]any, error) {
	fd := m.Descriptor().Fields().ByName("values")
	if fd == nil {
		return nil, fmt.Errorf("IntList missing `values` field")
	}
	list := m.Get(fd).List()
	out := make([]any, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		v := list.Get(i).Int()
		if wide {
			out = append(out, v)
		} else {
			out = append(out, int32(v))
		}
	}
	return out, nil
}

func bytesListValues(m protoreflect.Message) ([]any, error) {
	fd := m.Descriptor().Fields().ByName("values")
	if fd == nil {
		return nil, fmt.Errorf("BytesList missing `values` field")
	}
	list := m.Get(fd).List()
	out := make([]any, 0, list.Len())
	for i := 0; i < list.Len(); i++ {
		out = append(out, list.Get(i).Bytes())
	}
	return out, nil
}

// timestampFromMessage extracts a time.Time from a google.protobuf.Timestamp
// message accessed via protoreflect. We extract seconds + nanos rather than
// type-asserting to *timestamppb.Timestamp because dynamicpb messages don't
// type-assert into the concrete type — the translator works against both
// dynamic and concrete proto messages.
func timestampFromMessage(m protoreflect.Message) (time.Time, error) {
	if m == nil || !m.IsValid() {
		return time.Time{}, fmt.Errorf("nil timestamp")
	}
	desc := m.Descriptor()
	secF := desc.Fields().ByName("seconds")
	nanoF := desc.Fields().ByName("nanos")
	if secF == nil || nanoF == nil {
		return time.Time{}, fmt.Errorf("not a google.protobuf.Timestamp")
	}
	seconds := m.Get(secF).Int()
	nanos := int32(m.Get(nanoF).Int())
	return time.Unix(seconds, int64(nanos)).UTC(), nil
}

// sortByNumber orders set fields by proto field number. Stable +
// canonical; keeps the emitted SQL fragment deterministic across input
// field orderings.
func sortByNumber(s []setField) {
	// Insertion sort — N is tiny (predicates on one entity, ≤ tens of fields).
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1].fd.Number() > s[j].fd.Number(); j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
