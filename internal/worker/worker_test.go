package worker

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	b := 10 * time.Second
	if Backoff(b, 1) != 10*time.Second || Backoff(b, 2) != 20*time.Second || Backoff(b, 3) != 40*time.Second {
		t.Fatal("exponential growth broken")
	}
	if Backoff(b, 30) != time.Hour {
		t.Fatal("cap broken")
	}
}
