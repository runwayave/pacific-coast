package jobsdispatcher

import (
	"testing"
)

// The lease processor's full flow (drain + ExtendLease) is tested in
// the integration harness — exercising the actual PG UPDATE requires
// a live database. These unit tests pin the in-memory side of the
// contract: enqueue is non-blocking, full channels drop with a log,
// and the channel cap matches the documented constant.

func TestEnqueueLeaseBump_NonBlockingDrop(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)

	// Fill the channel to its declared cap so the next push hits the
	// non-blocking default: branch.
	for i := 0; i < cap(s.leasePendingCh); i++ {
		s.leasePendingCh <- int64(i)
	}
	if len(s.leasePendingCh) != cap(s.leasePendingCh) {
		t.Fatalf("setup: channel should be full; len=%d cap=%d",
			len(s.leasePendingCh), cap(s.leasePendingCh))
	}

	// This must not block — if it did, the test would hang and
	// the goroutine leak would surface as a timeout.
	d.enqueueLeaseBump(s, 999)

	if len(s.leasePendingCh) != cap(s.leasePendingCh) {
		t.Errorf("full channel push should not displace items; len=%d cap=%d",
			len(s.leasePendingCh), cap(s.leasePendingCh))
	}
}

func TestEnqueueLeaseBump_PushesOntoChannel(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)

	d.enqueueLeaseBump(s, 42)

	select {
	case got := <-s.leasePendingCh:
		if got != 42 {
			t.Errorf("got %d, want 42", got)
		}
	default:
		t.Fatal("expected job_id 42 on the channel")
	}
}

func TestLeasePendingCh_Capacity(t *testing.T) {
	// Pins the documented bound. Tests of "doesn't block under burst"
	// (TestEnqueueLeaseBump_NonBlockingDrop) depend on this cap not
	// being silently downsized in a future refactor.
	s := newTestSession(t)
	if cap(s.leasePendingCh) != leaseEnqueueCap {
		t.Errorf("leasePendingCh capacity = %d, want %d (leaseEnqueueCap constant)",
			cap(s.leasePendingCh), leaseEnqueueCap)
	}
}
