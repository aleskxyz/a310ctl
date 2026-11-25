package daemon

import (
	"testing"
	"time"
)

func TestSuspended(t *testing.T) {
	if got := suspended(2*time.Second, 2*time.Second); got != 0 {
		t.Fatalf("no sleep: %v", got)
	}
	if got := suspended(62*time.Second, 2*time.Second); got != time.Minute {
		t.Fatalf("one minute asleep: %v", got)
	}
	if got := suspended(2*time.Second, 3*time.Second); got != 0 {
		t.Fatalf("negative gap must be 0: %v", got)
	}
	detector := newSleepDetector()
	time.Sleep(20 * time.Millisecond)
	if slept := detector.check(); slept > 100*time.Millisecond {
		t.Fatalf("awake process reported %v asleep", slept)
	}
}
