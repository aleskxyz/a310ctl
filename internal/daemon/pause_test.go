package daemon

import (
	"testing"
	"time"
)

func TestCountPowerLoss(t *testing.T) {
	start := time.Unix(1000, 0)
	var times []time.Time
	var loop bool
	for i := 0; i < recoveryLoopLimit-1; i++ {
		if times, loop = countPowerLoss(times, start.Add(time.Duration(i)*10*time.Second)); loop {
			t.Fatalf("paused after %d recoveries", i+1)
		}
	}
	if _, loop = countPowerLoss(times, start.Add(50*time.Second)); !loop {
		t.Fatal("not paused after the limit within a minute")
	}
	// Spread out: older entries expire.
	times = nil
	for i := 0; i < 2*recoveryLoopLimit; i++ {
		if times, loop = countPowerLoss(times, start.Add(time.Duration(i)*20*time.Second)); loop {
			t.Fatalf("paused although recoveries were 20 s apart (i=%d)", i)
		}
	}
}
