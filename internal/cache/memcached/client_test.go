package memcached

import (
	"testing"
	"time"
)

func TestNew_RejectsEmptyAddrs(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatalf("expected error on empty Addrs")
	}
}

func TestDefaultConfig_Shape(t *testing.T) {
	c := DefaultConfig([]string{"localhost:11211"})
	if len(c.Addrs) != 1 || c.Addrs[0] != "localhost:11211" {
		t.Errorf("addrs: %v", c.Addrs)
	}
	if c.Timeout <= 0 {
		t.Errorf("Timeout should be positive, got %v", c.Timeout)
	}
	if c.MaxIdleConns <= 0 {
		t.Errorf("MaxIdleConns should be positive, got %d", c.MaxIdleConns)
	}
}

func TestTTLToExpiration(t *testing.T) {
	cases := []struct {
		ttl  time.Duration
		want int32
	}{
		{0, 0},
		{-1 * time.Second, 0},
		{1 * time.Second, 1},
		{30 * time.Second, 30},
		{10 * time.Minute, 600},
		{thirtyDays, 30 * 24 * 60 * 60},
		// Anything past 30 days clamps to exactly 30 days, otherwise memcached
		// interprets the value as an absolute Unix timestamp — a classic
		// memcached footgun the wrapper hides from generated code.
		{thirtyDays + time.Hour, 30 * 24 * 60 * 60},
		{365 * 24 * time.Hour, 30 * 24 * 60 * 60},
	}
	for _, c := range cases {
		if got := ttlToExpiration(c.ttl); got != c.want {
			t.Errorf("ttlToExpiration(%v) = %d want %d", c.ttl, got, c.want)
		}
	}
}

func TestInt64RoundTrip(t *testing.T) {
	cases := []int64{0, 1, -1, 42, -42, 1000000, -1000000, 1 << 62, -(1 << 62)}
	for _, c := range cases {
		got, err := parseInt64(formatInt64(c))
		if err != nil {
			t.Errorf("parseInt64(formatInt64(%d)) error: %v", c, err)
			continue
		}
		if got != c {
			t.Errorf("round trip %d → %d", c, got)
		}
	}
}

func TestParseInt64_Errors(t *testing.T) {
	// strconv.ParseInt accepts a leading '+' sign, which is a benign change
	// from the prior hand-rolled parser. The cases we still REJECT are the
	// genuinely malformed inputs that could let an attacker tamper with a
	// version pointer (review C9 / M1).
	cases := []string{"", "-", "abc", "12a", "-x"}
	for _, in := range cases {
		if _, err := parseInt64([]byte(in)); err == nil {
			t.Errorf("expected error on input %q", in)
		}
	}
}

// TestParseInt64_RejectsOverflow ensures the new strconv-backed parser
// surfaces ErrRange instead of silently wrapping (the prior parser would
// have wrapped 30-digit input to garbage).
func TestParseInt64_RejectsOverflow(t *testing.T) {
	overflow := "99999999999999999999999999999999"
	if _, err := parseInt64([]byte(overflow)); err == nil {
		t.Errorf("expected overflow error on %q", overflow)
	}
}

// Get / Set / Delete / CurrentVersion / SetVersion need a real memcached
// instance; their happy paths are covered in tests/integration (task #25).
// The shape (interface satisfaction, error translation) is verified at
// compile time by the var _ runtime.Cache = (*Client)(nil) declaration in
// client.go.
