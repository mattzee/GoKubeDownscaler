// Package health reports whether the scan loop is making progress, for the
// liveness and readiness endpoints.
//
// Liveness fails when this instance is running the scan loop and no cycle has
// completed within the stale threshold, so a wedged loop gets the pod
// restarted. An instance waiting for the leader lease is live: it is doing
// its job by standing by.
//
// Readiness fails until the first cycle completes on the instance running the
// scan loop. A standby instance is ready.
package health

import (
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

var (
	// ErrStale is returned by Live when no scan cycle completed within the stale threshold.
	ErrStale = errors.New("no scan cycle completed within the stale threshold")
	// ErrFirstCycle is returned by Ready until the first scan cycle completes.
	ErrFirstCycle = errors.New("first scan cycle has not completed")
)

// Tracker records the scan loop's progress. It is safe for concurrent use.
type Tracker struct {
	mutex      sync.RWMutex
	staleAfter time.Duration
	now        func() time.Time
	scanning   bool
	since      time.Time
	lastCycle  time.Time
}

// NewTracker returns a Tracker that reports stale after staleAfter without a completed cycle.
func NewTracker(staleAfter time.Duration) *Tracker {
	return &Tracker{staleAfter: staleAfter, now: time.Now}
}

// StaleAfter derives the stale threshold from the scan interval when override is zero:
// ten intervals, and never less than five minutes.
func StaleAfter(override, interval time.Duration) time.Duration {
	if override > 0 {
		return override
	}

	const (
		intervals = 10
		floor     = 5 * time.Minute
	)

	return max(intervals*interval, floor)
}

// StartedScanning marks this instance as running the scan loop.
func (t *Tracker) StartedScanning() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.scanning = true
	t.since = t.now()
	t.lastCycle = time.Time{}
}

// StoppedScanning marks this instance as no longer running the scan loop.
func (t *Tracker) StoppedScanning() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.scanning = false
}

// CycleCompleted records that a scan cycle completed now.
func (t *Tracker) CycleCompleted() {
	t.mutex.Lock()
	defer t.mutex.Unlock()

	t.lastCycle = t.now()
}

// Live returns an error when the scan loop has stalled.
func (t *Tracker) Live() error {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if !t.scanning {
		return nil
	}

	reference := t.lastCycle
	if reference.IsZero() {
		reference = t.since
	}

	if age := t.now().Sub(reference); age > t.staleAfter {
		return fmt.Errorf("%w: last progress %s ago, threshold %s", ErrStale, age.Round(time.Second), t.staleAfter)
	}

	return nil
}

// Ready returns an error until the first scan cycle completes on the scanning instance.
func (t *Tracker) Ready() error {
	t.mutex.RLock()
	defer t.mutex.RUnlock()

	if t.scanning && t.lastCycle.IsZero() {
		return ErrFirstCycle
	}

	return nil
}

// LivenessHandler serves Live as 200 or 503.
func (t *Tracker) LivenessHandler() http.HandlerFunc {
	return probeHandler(t.Live)
}

// ReadinessHandler serves Ready as 200 or 503.
func (t *Tracker) ReadinessHandler() http.HandlerFunc {
	return probeHandler(t.Ready)
}

func probeHandler(check func() error) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")

		if err := check(); err != nil {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte(err.Error() + "\n"))

			return
		}

		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("ok\n"))
	}
}
