package fuse

import (
	"testing"
	"time"
)

func TestFuseRespawnPlan(t *testing.T) {
	backoffs := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	max := 5

	cases := []struct {
		attempt int
		want    time.Duration
		giveUp  bool
	}{
		{0, 0, false},
		{1, time.Second, false},
		{2, 2 * time.Second, false},
		{3, 4 * time.Second, false},
		{4, 8 * time.Second, false},
		{5, 0, true},
		{-1, 0, true},
	}
	for _, tc := range cases {
		got, giveUp := fuseRespawnPlan(tc.attempt, backoffs, max)
		if giveUp != tc.giveUp || got != tc.want {
			t.Fatalf("attempt %d: got wait=%v giveUp=%v want wait=%v giveUp=%v",
				tc.attempt, got, giveUp, tc.want, tc.giveUp)
		}
	}
}
