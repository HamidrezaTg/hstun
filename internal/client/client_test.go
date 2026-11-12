package client

import (
	"testing"
	"time"
)

func TestStaggerDelay(t *testing.T) {
	max := 2 * time.Second
	if d := staggerDelay(max, 0); d != 0 {
		t.Fatalf("slot 0 must dial at once, got %v", d)
	}
	if d := staggerDelay(0, 3); d != 0 {
		t.Fatalf("stagger off must give 0, got %v", d)
	}
	seen := map[time.Duration]bool{}
	for i := 0; i < 500; i++ {
		d := staggerDelay(max, 1+i%7)
		if d < max/4 || d > max {
			t.Fatalf("delay %v outside [%v, %v]", d, max/4, max)
		}
		seen[d/(100*time.Millisecond)] = true
	}
	if len(seen) < 5 {
		t.Fatalf("delays are not spread out: %d distinct 100 ms buckets", len(seen))
	}
}
