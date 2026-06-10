package jobsdispatcher

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// newCheckpointTestDispatcher builds a Dispatcher with nil pool so
// the in-memory paths of handleCheckpoint run without requiring PG.
// The persistence layer is exercised in the integration tests.
func newCheckpointTestDispatcher(t *testing.T) *Dispatcher {
	t.Helper()
	return &Dispatcher{
		cfg: Config{
			HeartbeatBudget: 30 * time.Second,
			AckTimeoutMS:    15000,
			Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		},
	}
}

// withInflight installs one in-flight row on the session so the
// ownership check in handleCheckpoint passes.
func withInflight(s *session, jobID int64, jobName string) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	s.inflight[jobID] = inflightRow{
		jobID:        jobID,
		jobName:      jobName,
		dispatchedAt: time.Now(),
		leaseUntil:   time.Now().Add(30 * time.Second),
		ackBy:        time.Now().Add(15 * time.Second),
		ackReceived:  true,
	}
}

func TestCheckpoint_ClampsPctNegative(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	withInflight(s, 7, "vendor.ShopifyImport")

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: -42, Msg: "starting",
	})

	events := s.snapshotEvents()
	if len(events) == 0 {
		t.Fatal("expected at least one event after checkpoint")
	}
	last := events[len(events)-1]
	if !strings.Contains(last.Note, "0%") {
		t.Errorf("negative pct should clamp to 0; event note = %q", last.Note)
	}
}

func TestCheckpoint_ClampsPctOver100(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	withInflight(s, 7, "vendor.ShopifyImport")

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 250, Msg: "done",
	})

	events := s.snapshotEvents()
	if !strings.Contains(events[len(events)-1].Note, "100%") {
		t.Errorf("pct>100 should clamp to 100; event note = %q",
			events[len(events)-1].Note)
	}
}

func TestCheckpoint_TruncatesMsgPastMax(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	withInflight(s, 7, "vendor.ShopifyImport")

	longMsg := strings.Repeat("a", MaxCheckpointMsgChars+100)
	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 50, Msg: longMsg,
	})

	events := s.snapshotEvents()
	note := events[len(events)-1].Note
	// "50%: " prefix + truncated message; total length should be
	// 5 + MaxCheckpointMsgChars.
	if len(note) > 5+MaxCheckpointMsgChars {
		t.Errorf("msg should truncate at %d chars; got note len %d",
			MaxCheckpointMsgChars, len(note))
	}
}

func TestCheckpoint_UnknownRowIgnored(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	// No inflight row for jobID 999.

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 999, Pct: 50, Msg: "ghost",
	})

	events := s.snapshotEvents()
	for _, e := range events {
		if e.Kind == "checkpoint" {
			t.Errorf("unknown row should not produce a checkpoint event; got %+v", e)
		}
	}
}

func TestCheckpoint_ZeroJobIDIgnored(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 0, Pct: 50, Msg: "no id",
	})

	for _, e := range s.snapshotEvents() {
		if e.Kind == "checkpoint" {
			t.Errorf("zero job_id should be ignored; got %+v", e)
		}
	}
}

func TestCheckpoint_BumpsInMemoryLeaseClocks(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	withInflight(s, 7, "vendor.ShopifyImport")

	// Capture the pre-checkpoint clocks.
	s.inflightMu.Lock()
	pre := s.inflight[7].leaseUntil
	s.inflightMu.Unlock()

	// Force a small delay so time.Now() is detectably newer.
	time.Sleep(2 * time.Millisecond)

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 25, Msg: "quarter",
	})

	s.inflightMu.Lock()
	post := s.inflight[7].leaseUntil
	s.inflightMu.Unlock()

	if !post.After(pre) {
		t.Errorf("checkpoint should bump leaseUntil forward; pre=%v post=%v", pre, post)
	}
}

func TestCheckpoint_HonorsPerJobHeartbeatOverride(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	// Per-job override of 10 minutes, well above the 30-second global
	// budget configured in newCheckpointTestDispatcher.
	s.perJobHeartbeat = map[string]time.Duration{
		"vendor.ShopifyImport": 10 * time.Minute,
	}
	withInflight(s, 7, "vendor.ShopifyImport")

	before := time.Now()
	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 50, Msg: "halfway",
	})

	s.inflightMu.Lock()
	lease := s.inflight[7].leaseUntil
	s.inflightMu.Unlock()

	// Lease should be ~10m out, not ~30s. Allow slack for slow CI.
	gap := lease.Sub(before)
	if gap < 9*time.Minute || gap > 11*time.Minute {
		t.Errorf("expected lease ~10m out (per-job override), got %v", gap)
	}
}

func TestCheckpoint_HonorsShorterPerJobHeartbeat(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	// Shorter-than-global override. Pre-fix this was silently widened.
	s.perJobHeartbeat = map[string]time.Duration{
		"vendor.QuickJob": 5 * time.Second,
	}
	withInflight(s, 7, "vendor.QuickJob")

	before := time.Now()
	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 50, Msg: "halfway",
	})

	s.inflightMu.Lock()
	lease := s.inflight[7].leaseUntil
	s.inflightMu.Unlock()

	gap := lease.Sub(before)
	if gap < 4*time.Second || gap > 6*time.Second {
		t.Errorf("expected lease ~5s out (shorter per-job override), got %v", gap)
	}
}

func TestCheckpoint_UpdatesLastHeartbeatGauge(t *testing.T) {
	d := newCheckpointTestDispatcher(t)
	s := newTestSession(t)
	withInflight(s, 7, "vendor.ShopifyImport")

	// Reset lastHeartbeatNS to a definitely-stale value.
	staleTime := time.Now().Add(-time.Hour)
	s.lastHeartbeatNS.Store(staleTime.UnixNano())

	d.handleCheckpoint(context.Background(), s, &Checkpoint{
		JobID: 7, Pct: 50, Msg: "halfway",
	})

	fresh := s.lastHeartbeatAt()
	if !fresh.After(staleTime.Add(time.Minute)) {
		t.Errorf("checkpoint should bump lastHeartbeat forward; got %v (was %v)", fresh, staleTime)
	}
}
