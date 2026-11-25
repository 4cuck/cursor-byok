package execbridge

import (
	"strconv"
	"sync/atomic"
	"time"
)

// execIDClock reads the wall clock in nanoseconds. Tests replace it.
var execIDClock = func() int64 { return time.Now().UnixNano() }

// lastExecIDStamp is the most recent stamp NewExecID handed out.
var lastExecIDStamp atomic.Int64

// NewExecID returns prefix followed by a number that no other exec opened by
// this process shares.
//
// The number is the current time in nanoseconds, moved forward when the clock
// has not advanced since the previous call. The clock alone is not unique: on
// Windows it advances once per timer tick, roughly a millisecond, so execs
// opened back to back read the same value. Pending execs are keyed by this id.
// A repeated id replaces the earlier exec, its result is then dropped as
// unmatched, and the turn waits forever on a tool call that cannot finish.
func NewExecID(prefix string) string {
	return prefix + strconv.FormatInt(nextExecIDStamp(), 10)
}

func nextExecIDStamp() int64 {
	for {
		last := lastExecIDStamp.Load()
		stamp := execIDClock()
		if stamp <= last {
			stamp = last + 1
		}
		if lastExecIDStamp.CompareAndSwap(last, stamp) {
			return stamp
		}
	}
}
