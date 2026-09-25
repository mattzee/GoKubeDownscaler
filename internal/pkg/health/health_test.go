package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeClock struct{ current time.Time }

func (c *fakeClock) now() time.Time              { return c.current }
func (c *fakeClock) advance(delta time.Duration) { c.current = c.current.Add(delta) }

// newTestTracker returns a Tracker with a one-minute stale threshold on a fake clock.
func newTestTracker() (*Tracker, *fakeClock) {
	clock := &fakeClock{current: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	tracker := NewTracker(time.Minute)
	tracker.now = clock.now

	return tracker, clock
}

func TestStandbyIsLiveAndReady(t *testing.T) {
	t.Parallel()

	tracker, clock := newTestTracker()
	clock.advance(time.Hour)

	require.NoError(t, tracker.Live())
	require.NoError(t, tracker.Ready())
}

func TestNotReadyUntilFirstCycle(t *testing.T) {
	t.Parallel()

	tracker, clock := newTestTracker()
	tracker.StartedScanning()

	require.ErrorIs(t, tracker.Ready(), ErrFirstCycle)
	require.NoError(t, tracker.Live(), "live while the first cycle is within the threshold")

	clock.advance(30 * time.Second)
	tracker.CycleCompleted()

	require.NoError(t, tracker.Ready())
}

func TestStaleWithoutProgress(t *testing.T) {
	t.Parallel()

	tracker, clock := newTestTracker()
	tracker.StartedScanning()

	clock.advance(61 * time.Second)
	require.ErrorIs(t, tracker.Live(), ErrStale, "first cycle never completed")

	tracker.CycleCompleted()
	require.NoError(t, tracker.Live())

	clock.advance(60 * time.Second)
	require.NoError(t, tracker.Live(), "exactly at the threshold is still live")

	clock.advance(time.Second)
	require.ErrorIs(t, tracker.Live(), ErrStale)
}

func TestStoppedScanningIsLive(t *testing.T) {
	t.Parallel()

	tracker, clock := newTestTracker()
	tracker.StartedScanning()
	clock.advance(time.Hour)
	tracker.StoppedScanning()

	require.NoError(t, tracker.Live())
	require.NoError(t, tracker.Ready())
}

func TestRestartedScanningResetsProgress(t *testing.T) {
	t.Parallel()

	tracker, clock := newTestTracker()
	tracker.StartedScanning()
	tracker.CycleCompleted()
	tracker.StoppedScanning()

	clock.advance(time.Hour)
	tracker.StartedScanning()

	require.NoError(t, tracker.Live(), "a new term starts its own clock")
	require.ErrorIs(t, tracker.Ready(), ErrFirstCycle)
}

func TestStaleAfter(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5*time.Minute, StaleAfter(0, 5*time.Second), "floor")
	assert.Equal(t, 10*time.Minute, StaleAfter(0, time.Minute), "ten intervals")
	assert.Equal(t, 90*time.Second, StaleAfter(90*time.Second, time.Hour), "override wins")
}

func TestHandlers(t *testing.T) {
	t.Parallel()

	tracker, _ := newTestTracker()
	tracker.StartedScanning()

	recorder := httptest.NewRecorder()
	tracker.ReadinessHandler()(recorder, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), ErrFirstCycle.Error())

	recorder = httptest.NewRecorder()
	tracker.LivenessHandler()(recorder, httptest.NewRequest(http.MethodGet, "/healthz", http.NoBody))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Equal(t, "ok\n", recorder.Body.String())
}
