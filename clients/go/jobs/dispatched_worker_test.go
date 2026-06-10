package jobs

import (
	"context"
	"strings"
	"testing"
)

// newTestDispatchedWorker constructs a minimal DispatchedWorker with
// the channel cap structure we want to exercise. No gRPC stream
// involvement — the tests target the channel routing + checkpointer
// contract.
func newTestDispatchedWorker(t *testing.T) *DispatchedWorker {
	t.Helper()
	return &DispatchedWorker{
		cfg:    ServerConfig{MaxInFlight: 4, PodID: "test"},
		ctrlCh: make(chan *WorkerEnvelope, 4+8),
		dataCh: make(chan *WorkerEnvelope, 4*2),
	}
}

// TestStreamCheckpointer_RoutesViaCtrlCh pins the load-bearing
// guarantee: Checkpoint envelopes go through the priority control
// channel, never the data plane. If a future refactor pushes
// Checkpoint to dataCh, the stale-heartbeat regression bites again.
func TestStreamCheckpointer_RoutesViaCtrlCh(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 42)

	if err := c.Report(context.Background(), 50, "halfway"); err != nil {
		t.Fatalf("Report: %v", err)
	}

	select {
	case env := <-w.ctrlCh:
		if env.Checkpoint == nil {
			t.Fatalf("ctrlCh envelope missing Checkpoint payload")
		}
		if env.Checkpoint.JobID != 42 || env.Checkpoint.Pct != 50 || env.Checkpoint.Msg != "halfway" {
			t.Errorf("unexpected checkpoint payload: %+v", env.Checkpoint)
		}
	default:
		t.Fatal("expected checkpoint on ctrlCh")
	}

	select {
	case env := <-w.dataCh:
		t.Errorf("dataCh must NOT receive Checkpoint; got %+v", env)
	default:
	}
}

func TestStreamCheckpointer_ClampsPct(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 7)

	cases := []struct {
		in, want int
	}{
		{-5, 0},
		{0, 0},
		{50, 50},
		{100, 100},
		{1234, 100},
	}
	for _, tc := range cases {
		drainCtrl(w)
		_ = c.Report(context.Background(), tc.in, "x")
		env := <-w.ctrlCh
		if env.Checkpoint.Pct != tc.want {
			t.Errorf("Pct=%d clamped to %d, want %d",
				tc.in, env.Checkpoint.Pct, tc.want)
		}
	}
}

func TestStreamCheckpointer_TruncatesMsg(t *testing.T) {
	w := newTestDispatchedWorker(t)
	c := newStreamCheckpointer(w, 7)
	long := strings.Repeat("x", MaxCheckpointMsgChars+50)

	_ = c.Report(context.Background(), 50, long)
	env := <-w.ctrlCh
	if len(env.Checkpoint.Msg) != MaxCheckpointMsgChars {
		t.Errorf("Msg length = %d, want %d",
			len(env.Checkpoint.Msg), MaxCheckpointMsgChars)
	}
}

func TestCheckpoint_NoOpWithoutWorkerCtx(t *testing.T) {
	// The package-level Checkpoint function must be a no-op when no
	// Checkpointer is wired into the ctx — that's the "safe in unit
	// tests" contract documented on the API.
	if err := Checkpoint(context.Background(), 50, "msg"); err != nil {
		t.Errorf("Checkpoint on bare ctx should return nil; got %v", err)
	}
}

func TestCheckpoint_RoutesThroughInstalledCheckpointer(t *testing.T) {
	w := newTestDispatchedWorker(t)
	ctx := withCheckpointer(context.Background(), newStreamCheckpointer(w, 99))

	if err := Checkpoint(ctx, 25, "quarter"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	env := <-w.ctrlCh
	if env.Checkpoint.JobID != 99 || env.Checkpoint.Pct != 25 {
		t.Errorf("envelope routed wrong: %+v", env.Checkpoint)
	}
}

// drainCtrl removes any pending envelopes from ctrlCh between sub-test
// iterations of the clamping test.
func drainCtrl(w *DispatchedWorker) {
	for {
		select {
		case <-w.ctrlCh:
		default:
			return
		}
	}
}
