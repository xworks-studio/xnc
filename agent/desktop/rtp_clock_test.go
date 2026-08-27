package desktop

import (
	"errors"
	"math"
	"testing"
)

func TestRTPClockPlacesGapOnCurrentFrame(t *testing.T) {
	c := newRTPClock(1000)
	a, err := c.Timestamp(1_000_000)
	if err != nil {
		t.Fatalf("first timestamp: %v", err)
	}
	b, err := c.Timestamp(31_000_000)
	if err != nil {
		t.Fatalf("second timestamp: %v", err)
	}
	if gap := b - a; gap != 2_700_000 {
		t.Fatalf("gap=%d, want 2700000", gap)
	}
}

func TestRTPClockRejectsNonIncreasingPresentationTime(t *testing.T) {
	for _, tc := range []struct {
		name string
		next uint64
	}{
		{name: "equal", next: 2},
		{name: "regression", next: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRTPClock(0)
			if _, err := c.Timestamp(2); err != nil {
				t.Fatalf("first timestamp: %v", err)
			}
			if _, err := c.Timestamp(tc.next); !errors.Is(err, errRTPTimeRegression) {
				t.Fatalf("Timestamp(%d) error=%v, want %v", tc.next, err, errRTPTimeRegression)
			}
		})
	}
}

func TestRTPClockLargeDeltaRoundsWithoutOverflowAndWraps(t *testing.T) {
	const base uint32 = 4_000_000_000
	c := newRTPClock(base)
	if got, err := c.Timestamp(0); err != nil || got != base {
		t.Fatalf("first timestamp=(%d, %v), want (%d, nil)", got, err, base)
	}

	// round(MaxUint64 microseconds * 90kHz / 1e6) modulo 2^32,
	// then add base modulo 2^32. The hand-derived result catches a
	// uint64 overflow in a direct (delta*9+50)/100 implementation.
	const want uint32 = 2_453_811_773
	got, err := c.Timestamp(math.MaxUint64)
	if err != nil {
		t.Fatalf("large timestamp: %v", err)
	}
	if got != want {
		t.Fatalf("large timestamp=%d, want %d", got, want)
	}
}

func TestPublisherRejectsUnpacketizableAnnexB(t *testing.T) {
	p, err := NewPublisher(PublisherConfig{})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	p.connReady.Store(true)
	p.stateMu.Lock()
	p.started = true
	p.stateMu.Unlock()

	err = p.WriteFrame(Frame{PresentMonoUs: 1, AU: []byte{0, 0, 0, 1}})
	if err == nil {
		t.Fatal("WriteFrame accepted Annex-B data that produced no RTP packets")
	}
}
