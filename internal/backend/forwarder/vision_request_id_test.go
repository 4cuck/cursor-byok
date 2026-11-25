package forwarder

import "testing"

// The images of one message are described in parallel. Each call's log lines
// and debug artifacts are filed under its request id, so calls started at the
// same instant need different ids. An id taken from the clock repeats: the
// clock stands still for up to a millisecond at a time.
func TestVisionProxyRequestIDsDifferBetweenCallsStartedTogether(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := newVisionProxyRequestID()
		if seen[id] {
			t.Fatalf("request id %q was handed to two image description calls", id)
		}
		seen[id] = true
	}
}
