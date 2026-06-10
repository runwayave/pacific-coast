package jobsdispatcher

import (
	"testing"
	"time"
)

func newTestSession(t *testing.T) *session {
	t.Helper()
	open := &OpenSession{
		Queue:       "default",
		JobNames:    []string{"vendor.ShopifyImport"},
		MaxInFlight: 4,
		PodID:       "test-pod",
	}
	return newSession(open, "vendor", nil, nil)
}

func TestSession_AvailableSlotsStartsAtMax(t *testing.T) {
	s := newTestSession(t)
	if got := s.availableSlots(); got != 4 {
		t.Errorf("availableSlots = %d, want 4", got)
	}
}

func TestSession_RecordDispatchDecrementsAvailable(t *testing.T) {
	s := newTestSession(t)
	d := &Dispatch{JobID: 1, JobName: "vendor.ShopifyImport"}
	if !s.recordDispatch(d, time.Now().Add(time.Minute), time.Now().Add(30*time.Second)) {
		t.Fatal("recordDispatch returned false on first push")
	}
	if got := s.availableSlots(); got != 3 {
		t.Errorf("after one dispatch, availableSlots = %d, want 3", got)
	}
	if got := s.inflightCount(); got != 1 {
		t.Errorf("inflightCount = %d, want 1", got)
	}
}

func TestSession_RecordDispatchFullOutboxReturnsFalse(t *testing.T) {
	s := newTestSession(t)
	// outbox capacity is MaxInFlight + 4 = 8. Fill it.
	for i := 0; i < 8; i++ {
		ok := s.recordDispatch(&Dispatch{JobID: int64(i)}, time.Now().Add(time.Minute), time.Now().Add(30*time.Second))
		if !ok {
			t.Fatalf("dispatch %d should succeed; capacity is 8", i)
		}
	}
	// 9th should fail (outbox full).
	if s.recordDispatch(&Dispatch{JobID: 999}, time.Now().Add(time.Minute), time.Now().Add(30*time.Second)) {
		t.Fatal("dispatch should fail when outbox full")
	}
}

func TestSession_RecordAckSetsFlag(t *testing.T) {
	s := newTestSession(t)
	s.recordDispatch(&Dispatch{JobID: 42}, time.Now().Add(time.Minute), time.Now().Add(30*time.Second))
	ok, lat := s.recordAck(42)
	if !ok {
		t.Fatal("recordAck should succeed for known id")
	}
	if lat <= 0 {
		t.Errorf("ack latency should be > 0, got %v", lat)
	}
	// Second Ack is a no-op (idempotent).
	ok2, lat2 := s.recordAck(42)
	if !ok2 {
		t.Fatal("second recordAck should still return true")
	}
	if lat2 != 0 {
		t.Errorf("duplicate ack latency should be 0, got %v", lat2)
	}
}

func TestSession_RecordAckUnknownIDReturnsFalse(t *testing.T) {
	s := newTestSession(t)
	if ok, _ := s.recordAck(123); ok {
		t.Fatal("recordAck for unknown id should return false")
	}
}

func TestSession_RemoveInflightDecrementsCount(t *testing.T) {
	s := newTestSession(t)
	s.recordDispatch(&Dispatch{JobID: 1}, time.Now().Add(time.Minute), time.Now().Add(30*time.Second))
	s.recordDispatch(&Dispatch{JobID: 2}, time.Now().Add(time.Minute), time.Now().Add(30*time.Second))
	if got := s.inflightCount(); got != 2 {
		t.Fatalf("setup: inflightCount = %d, want 2", got)
	}
	if _, ok := s.removeInflight(1); !ok {
		t.Fatal("removeInflight should succeed for known id")
	}
	if got := s.inflightCount(); got != 1 {
		t.Errorf("after removal, inflightCount = %d, want 1", got)
	}
}

func TestSession_FindExpiredAcksOnlyReturnsExpired(t *testing.T) {
	s := newTestSession(t)
	now := time.Now()
	s.recordDispatch(&Dispatch{JobID: 1}, now.Add(time.Minute), now.Add(-time.Second)) // expired
	s.recordDispatch(&Dispatch{JobID: 2}, now.Add(time.Minute), now.Add(time.Minute))  // fresh
	expired := s.findExpiredAcks(now)
	if len(expired) != 1 || expired[0] != 1 {
		t.Errorf("expired = %v, want [1]", expired)
	}

	// Acked rows don't show up as expired even if past deadline.
	s.recordAck(1)
	expired = s.findExpiredAcks(now)
	if len(expired) != 0 {
		t.Errorf("after Ack, expired = %v, want []", expired)
	}
}

func TestSession_MarkDrainedZerosAvailableSlots(t *testing.T) {
	s := newTestSession(t)
	if got := s.availableSlots(); got != 4 {
		t.Fatalf("setup: availableSlots = %d", got)
	}
	s.markDrained()
	if got := s.availableSlots(); got != 0 {
		t.Errorf("drained session availableSlots = %d, want 0", got)
	}
}

func TestSession_MaxInFlightClamped(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int
	}{
		{"below_min", 0, MinMaxInFlight},
		{"at_min", MinMaxInFlight, MinMaxInFlight},
		{"valid", 8, 8},
		{"at_max", MaxMaxInFlight, MaxMaxInFlight},
		{"above_max", 9999, MaxMaxInFlight},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newSession(&OpenSession{Queue: "q", JobNames: []string{"x"}, MaxInFlight: tc.in}, "c", nil, nil)
			if s.maxInFlight != tc.want {
				t.Errorf("MaxInFlight=%d → got %d, want %d", tc.in, s.maxInFlight, tc.want)
			}
		})
	}
}

func TestSession_AppendEventCapsAtSessionEventCap(t *testing.T) {
	s := newTestSession(t)
	for i := 0; i < sessionEventCap+10; i++ {
		s.appendEvent(sessionEvent{At: time.Now(), Kind: "test"})
	}
	got := s.snapshotEvents()
	if len(got) != sessionEventCap {
		t.Errorf("len(events) = %d, want %d", len(got), sessionEventCap)
	}
}

func TestSession_HandlesJob(t *testing.T) {
	s := newTestSession(t)
	if !s.handlesJob("vendor.ShopifyImport") {
		t.Error("session should handle declared job")
	}
	if s.handlesJob("consumer.SweepExpired") {
		t.Error("session should not handle undeclared job")
	}
}

func TestSession_ClaimedByIncludesSessionID(t *testing.T) {
	s := newTestSession(t)
	cb := s.claimedBy()
	if cb == "" {
		t.Fatal("claimedBy returned empty")
	}
	if cb[:len("dispatcher/")] != "dispatcher/" {
		t.Errorf("claimedBy should start with 'dispatcher/', got %q", cb)
	}
}

func TestCursor_RoundRobinIncrements(t *testing.T) {
	c := newCursor()
	if got := c.next(); got != 0 {
		t.Errorf("first next = %d, want 0", got)
	}
	if got := c.next(); got != 1 {
		t.Errorf("second next = %d, want 1", got)
	}
}
