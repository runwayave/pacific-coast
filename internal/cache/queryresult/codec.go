package queryresult

import (
	"encoding/binary"
	"fmt"
)

// Tier-2 cache value codec. The stored byte sequence is one of:
//
//	v1: [0x01][repeat: uvarint(len), len bytes of PK]
//	v2: [0x02][uvarint(token-len)][token-bytes][repeat: uvarint(len), len bytes of PK]
//
// v2 carries the keyset `next_page_token` alongside the PK list so a
// cache hit can echo it back without re-running the underlying SQL.
// v1 entries are still readable; they decode as having no next-page
// cursor. New writes always emit v2.
//
// Why not protobuf: PK lists are potentially long (≤ 1000); a
// proto-encoded `repeated string` adds 2 bytes per entry for the field
// tag, and Marshal/Unmarshal allocates per call. The hand-rolled
// format runs at memcpy speed with one allocation per encode.

const (
	codecVersion1 byte = 1
	codecVersion2 byte = 2
)

// payload is the in-memory shape of a decoded cache entry. NextPageToken
// is the empty string for entries that predate keyset pagination
// and for v2 entries that ended on the last page.
type payload struct {
	NextPageToken string
	PKs           []string
}

func encodePayload(p payload) ([]byte, error) {
	if len(p.PKs) > MaxPKListSize {
		return nil, fmt.Errorf("queryresult: pk list length %d exceeds %d", len(p.PKs), MaxPKListSize)
	}
	if len(p.NextPageToken) > MaxNextPageTokenLen {
		return nil, fmt.Errorf("queryresult: next_page_token length %d exceeds %d", len(p.NextPageToken), MaxNextPageTokenLen)
	}
	size := 1 + 10 + len(p.NextPageToken)
	for _, pk := range p.PKs {
		size += 10 + len(pk)
	}
	out := make([]byte, 0, size)
	out = append(out, codecVersion2)
	var lenBuf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(lenBuf[:], uint64(len(p.NextPageToken)))
	out = append(out, lenBuf[:n]...)
	out = append(out, p.NextPageToken...)
	for _, pk := range p.PKs {
		n := binary.PutUvarint(lenBuf[:], uint64(len(pk)))
		out = append(out, lenBuf[:n]...)
		out = append(out, pk...)
	}
	return out, nil
}

func decodePayload(b []byte) (payload, error) {
	if len(b) == 0 {
		return payload{}, fmt.Errorf("queryresult: empty value")
	}
	switch b[0] {
	case codecVersion1:
		pks, err := decodeV1PKList(b[1:])
		return payload{PKs: pks}, err
	case codecVersion2:
		return decodeV2(b[1:])
	default:
		return payload{}, fmt.Errorf("queryresult: unknown codec version %d", b[0])
	}
}

func decodeV1PKList(b []byte) ([]string, error) {
	var out []string
	i := 0
	for i < len(b) {
		n, consumed := binary.Uvarint(b[i:])
		if consumed <= 0 {
			return nil, fmt.Errorf("queryresult: malformed length varint at %d", i)
		}
		i += consumed
		if n > MaxPKByteLen {
			return nil, fmt.Errorf("queryresult: pk length %d exceeds %d", n, MaxPKByteLen)
		}
		if i+int(n) > len(b) {
			return nil, fmt.Errorf("queryresult: truncated pk at %d (need %d bytes, have %d)", i, n, len(b)-i)
		}
		out = append(out, string(b[i:i+int(n)]))
		i += int(n)
		if len(out) > MaxPKListSize {
			return nil, fmt.Errorf("queryresult: pk list exceeds %d", MaxPKListSize)
		}
	}
	return out, nil
}

func decodeV2(b []byte) (payload, error) {
	tokLen, consumed := binary.Uvarint(b)
	if consumed <= 0 {
		return payload{}, fmt.Errorf("queryresult: malformed token-length varint")
	}
	if tokLen > MaxNextPageTokenLen {
		return payload{}, fmt.Errorf("queryresult: token length %d exceeds %d", tokLen, MaxNextPageTokenLen)
	}
	if consumed+int(tokLen) > len(b) {
		return payload{}, fmt.Errorf("queryresult: truncated token")
	}
	tok := string(b[consumed : consumed+int(tokLen)])
	pks, err := decodeV1PKList(b[consumed+int(tokLen):])
	if err != nil {
		return payload{}, err
	}
	return payload{NextPageToken: tok, PKs: pks}, nil
}

// Tunable caps on what the codec will accept. These limits exist so a
// memcached-network attacker who substitutes a tampered value can't
// allocate an arbitrary amount of memory in the reader.
const (
	MaxPKListSize       = 2000 // 2x the LIMIT cap leaves headroom for keyset peek
	MaxPKByteLen        = 512  // a PK longer than this is implausible
	MaxNextPageTokenLen = 4096 // base64url-encoded protobuf cursor; well above the typical 100-200 bytes
)
