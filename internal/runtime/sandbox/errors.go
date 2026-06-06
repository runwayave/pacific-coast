package sandbox

import (
	"errors"
	"fmt"
)

// errBackendUnknown is the typed error New returns when Options.Backend
// is not one of BackendAuto / BackendSim / BackendEmbedded.
func errBackendUnknown(b Backend) error {
	return fmt.Errorf("sandbox: unknown backend %q", b)
}

// ErrUnknownEntity is returned by Inspect when the qualified name
// passed in doesn't match any registered table. Callers can errors.Is
// this to distinguish "typo" from "real error".
var ErrUnknownEntity = errors.New("sandbox: unknown entity")

// ErrUnknownColumn is returned by Inspect when a Find predicate
// references a column that doesn't exist on the table.
var ErrUnknownColumn = errors.New("sandbox: unknown column")
