package desktop

import "errors"

var errRTPTimeRegression = errors.New("desktop: RTP presentation time did not increase")

type rtpClock struct {
	base    uint32
	first   uint64
	last    uint64
	started bool
}

func newRTPClock(base uint32) *rtpClock {
	return &rtpClock{base: base}
}

func (c *rtpClock) Timestamp(presentMonoUs uint64) (uint32, error) {
	if !c.started {
		c.first = presentMonoUs
		c.last = presentMonoUs
		c.started = true
		return c.base, nil
	}
	if presentMonoUs <= c.last {
		return 0, errRTPTimeRegression
	}

	c.last = presentMonoUs
	delta := presentMonoUs - c.first
	// One 90 kHz tick is 100/9 microseconds. Splitting at 100 us keeps
	// the multiplication below uint64 capacity for every possible delta,
	// while the remainder term preserves nearest-tick rounding.
	ticks := (delta/100)*9 + ((delta%100)*9+50)/100
	return c.base + uint32(ticks), nil
}
