package sandbox

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"time"

	"github.com/rachitkumar205/atlantis/internal/runtime/sandbox/sim"
)

// snapshotMagic is the wire-format tag included in every snapshot. A
// format change (new row-value types, different layout) bumps the
// Format field; older blobs refuse to load with ErrSnapshotFormat.
const snapshotMagic = "atl-sandbox-snapshot"

// snapshotFormat is the current wire-format version. Bump on any
// breaking change to encodedSnapshot.
const snapshotFormat = 1

// ErrSnapshotSchemaMismatch is returned by Restore when the saved
// catalog signature doesn't match the current sandbox's signature.
// Callers can errors.Is to switch on "old snapshot, schema evolved"
// vs other restore failures.
var ErrSnapshotSchemaMismatch = errors.New("sandbox: snapshot schema does not match current catalog")

// ErrSnapshotFormat is returned by Restore when the blob's format
// version is older or newer than this build understands.
var ErrSnapshotFormat = errors.New("sandbox: snapshot format unsupported")

// encodedSnapshot is the on-wire shape. Kept lowercase so consumers
// can't poke at its layout — Restore is the only path back to a live
// Sandbox.
type encodedSnapshot struct {
	Magic     string
	Format    int
	SavedAt   time.Time
	Signature string
	Tables    map[string][]encodedRow
}

// encodedRow is one row's column values. We store []any verbatim
// (gob handles int64 / string / bool / time.Time / []byte) — sim
// already constrains values to those types, so the surface stays
// small and the wire shape stays stable across additions.
type encodedRow struct {
	Cols []any
}

// Snapshot serializes the entire sandbox state to a single blob. The
// returned bytes carry: the wire-format version, the catalog signature
// at write time, the wall-clock save timestamp, and the current rows
// of every registered table.
//
// Snapshot produces a portable, cross-process blob. The in-process
// O(num_tables) variant lives in marks.go (Mark / RestoreTo) and is
// the right primitive for agent-loop rewinds within a single process;
// Snapshot is the right primitive for persistence and IPC.
//
// Cost is O(num rows); the gob encoder dominates.
func (s *Sandbox) Snapshot() ([]byte, error) {
	enc := encodedSnapshot{
		Magic:     snapshotMagic,
		Format:    snapshotFormat,
		SavedAt:   s.opts.Clock(),
		Signature: s.pool.Catalog().Signature(),
		Tables:    map[string][]encodedRow{},
	}
	for qn, rows := range s.pool.ExportRows() {
		out := make([]encodedRow, len(rows))
		for i, r := range rows {
			out[i] = encodedRow{Cols: []any(r)}
		}
		enc.Tables[qn] = out
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&enc); err != nil {
		return nil, fmt.Errorf("sandbox snapshot: encode: %w", err)
	}
	return buf.Bytes(), nil
}

// Restore re-installs a previously captured snapshot. Returns
// ErrSnapshotFormat for wire-format / magic mismatches and
// ErrSnapshotSchemaMismatch when the blob's catalog signature differs
// from the live sandbox's — the second case means the schema has
// evolved since the snapshot was taken; the caller decides whether to
// migrate the snapshot or discard it.
//
// Restore is destructive: tables that were live before Restore are
// replaced wholesale; tables present in the live catalog but absent
// from the snapshot are cleared. There is no merge mode.
func (s *Sandbox) Restore(blob []byte) error {
	var dec encodedSnapshot
	if err := gob.NewDecoder(bytes.NewReader(blob)).Decode(&dec); err != nil {
		return fmt.Errorf("sandbox restore: decode: %w", err)
	}
	if dec.Magic != snapshotMagic {
		return fmt.Errorf("%w: missing/invalid magic", ErrSnapshotFormat)
	}
	if dec.Format != snapshotFormat {
		return fmt.Errorf("%w: blob format %d, want %d", ErrSnapshotFormat, dec.Format, snapshotFormat)
	}
	if dec.Signature != s.pool.Catalog().Signature() {
		return ErrSnapshotSchemaMismatch
	}
	imported := map[string][]sim.Row{}
	for qn, rows := range dec.Tables {
		out := make([]sim.Row, len(rows))
		for i, r := range rows {
			out[i] = sim.Row(r.Cols)
		}
		imported[qn] = out
	}
	return s.pool.ImportRows(imported)
}

// init registers concrete types whose only contact with the encoder
// is via interface{}. encoding/gob needs the registration so it can
// recover the original type on decode.
func init() {
	gob.Register(int64(0))
	gob.Register("")
	gob.Register(true)
	gob.Register(time.Time{})
	gob.Register([]byte(nil))
}
