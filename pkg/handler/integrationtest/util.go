package integrationtest

import "time"

// timeAfter returns a one-shot channel that fires after the
// shutdown grace period. Pulled out so tests on slow runners can
// override it via a build flag if needed.
func timeAfter() <-chan time.Time {
	return time.After(5 * time.Second)
}
