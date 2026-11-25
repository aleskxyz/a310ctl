package daemon

import (
	"time"

	"golang.org/x/sys/unix"
)

// sleepDetector measures time the system spent suspended: CLOCK_BOOTTIME keeps running
// during suspend, Go's monotonic clock (CLOCK_MONOTONIC) does not.
type sleepDetector struct {
	start     time.Time
	boot      time.Duration
	monotonic time.Duration
}

func bootTime() time.Duration {
	var spec unix.Timespec
	if unix.ClockGettime(unix.CLOCK_BOOTTIME, &spec) != nil {
		return 0
	}
	return time.Duration(spec.Nano())
}

func newSleepDetector() *sleepDetector {
	return &sleepDetector{start: time.Now(), boot: bootTime()}
}

// suspended returns how much of an interval was spent suspended.
func suspended(bootDelta, monotonicDelta time.Duration) time.Duration {
	if gap := bootDelta - monotonicDelta; gap > 0 {
		return gap
	}
	return 0
}

// check returns the suspended time since the previous check.
func (detector *sleepDetector) check() time.Duration {
	boot, monotonic := bootTime(), time.Since(detector.start)
	slept := suspended(boot-detector.boot, monotonic-detector.monotonic)
	detector.boot, detector.monotonic = boot, monotonic
	return slept
}
