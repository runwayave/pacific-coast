package queryresult

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"

	"google.golang.org/protobuf/proto"
)

// Hash derives the tier-2 cache-key digest from the request inputs.
// The canonical layout below is the authoritative — keep it in sync
// with the codegen-side hash construction.
//
// Inputs:
//   - entityID:   "namespace.Entity" (e.g. "consumer.Account")
//   - filter:     the request's <Entity>Filter proto. Nil → no-filter
//     contribution (the bytes "0:" appear in the hash).
//   - orders:     repeated <Entity>OrderBy proto messages. Each one's
//     deterministic-marshaled bytes contribute to the hash
//     in the caller-supplied order. The caller-supplied
//     order matters — orderby is semantically ordered.
//   - limit:      varint-encoded request limit.
//   - pageToken:  raw bytes of the opaque cursor (treated as []byte).
//   - fields:     google.protobuf.FieldMask proto. Nil → no-mask
//     contribution (which the form treats as
//     "all fields"; codegen produces a default empty mask
//     for that case so the hash is stable).
//   - includes:   the request's `repeated <Entity>Include includes` —
//     sorted by numeric value before hashing for canonical
//     stability.
//   - generation: the per-entity counter value from
//     (*Cache).Generation. Folding the counter into the
//     hash key is what makes a generation-bump invalidate
//     every cached entry for the entity at once.
//
// Returns the 22-char base64url-encoded 128-bit (16-byte) prefix of a
// sha256 digest. 128 bits is well past collision-resistant for the
// cache size in play; truncation keeps memcached keys short.
func Hash(
	entityID string,
	filter proto.Message,
	orders []proto.Message,
	limit int32,
	pageToken []byte,
	fields proto.Message,
	includes []int32,
	generation int64,
) (string, error) {
	h := sha256.New()

	// Each segment is prefixed with its length-varint so two payloads
	// with the same concatenated bytes but different segment boundaries
	// can't hash to the same value (canonical-form rule).
	if err := writeSegmentString(h, entityID); err != nil {
		return "", err
	}
	if err := writeSegmentProto(h, filter); err != nil {
		return "", fmt.Errorf("hash filter: %w", err)
	}
	if err := writeSegmentInt(h, int64(len(orders))); err != nil {
		return "", err
	}
	for i, o := range orders {
		if err := writeSegmentProto(h, o); err != nil {
			return "", fmt.Errorf("hash order[%d]: %w", i, err)
		}
	}
	if err := writeSegmentInt(h, int64(limit)); err != nil {
		return "", err
	}
	if err := writeSegmentBytes(h, pageToken); err != nil {
		return "", err
	}
	if err := writeSegmentProto(h, fields); err != nil {
		return "", fmt.Errorf("hash fields: %w", err)
	}
	if err := writeSegmentInt(h, int64(len(includes))); err != nil {
		return "", err
	}
	// Includes are sorted numerically before contribution so the hash is
	// order-independent across equivalent caller inputs. The caller passes
	// them as-is; we sort a local copy.
	sortedIncludes := make([]int32, len(includes))
	copy(sortedIncludes, includes)
	sortInt32(sortedIncludes)
	for _, v := range sortedIncludes {
		if err := writeSegmentInt(h, int64(v)); err != nil {
			return "", err
		}
	}
	if err := writeSegmentInt(h, generation); err != nil {
		return "", err
	}

	sum := h.Sum(nil)
	// Truncate to 16 bytes (128 bits), then base64url-no-pad encode →
	// 22 chars. Fits well inside memcached's 250-byte key limit even
	// alongside the entity name prefix.
	return base64.RawURLEncoding.EncodeToString(sum[:16]), nil
}

// writeSegmentString writes one length-prefixed UTF-8 segment.
func writeSegmentString(h interface{ Write([]byte) (int, error) }, s string) error {
	return writeSegmentBytes(h, []byte(s))
}

// writeSegmentBytes writes one length-prefixed binary segment.
func writeSegmentBytes(h interface{ Write([]byte) (int, error) }, b []byte) error {
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(b)))
	if _, err := h.Write(lenBuf[:n]); err != nil {
		return err
	}
	_, err := h.Write(b)
	return err
}

// writeSegmentInt writes one signed varint segment.
func writeSegmentInt(h interface{ Write([]byte) (int, error) }, v int64) error {
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutVarint(buf[:], v)
	_, err := h.Write(buf[:n])
	return err
}

// writeSegmentProto deterministically marshals a proto message and
// writes it as a length-prefixed segment. Nil messages write a zero-
// length segment (which is still distinct from any non-nil empty
// message thanks to the varint prefix and the rest of the canonical
// frame).
func writeSegmentProto(h interface{ Write([]byte) (int, error) }, m proto.Message) error {
	if m == nil {
		return writeSegmentBytes(h, nil)
	}
	// Deterministic = true is essential — proto3 maps and `repeated
	// google.protobuf.Value` have nondeterministic field order without
	// it, breaking the hash on retries.
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		return err
	}
	return writeSegmentBytes(h, b)
}

// sortInt32 is the smallest in-package int32 sort. The slice is tiny
// (one entry per requested Include enum variant; cap is the number
// of inbound FKs per entity, typically < 10).
func sortInt32(s []int32) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
