package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/quartz"
	"github.com/stretchr/testify/require"
)

func TestLogQueuer_Stalled(t *testing.T) {
	t.Parallel()

	clock := quartz.NewMock(t)
	lq := &logQueuer{
		clock:           clock,
		livenessTimeout: 60 * time.Second,
	}

	// Before any progress, the loop is considered stalled.
	require.True(t, lq.stalled())

	// Record progress at the current mock time.
	lq.markProgress()
	require.False(t, lq.stalled())

	// Advancing the clock beyond livenessTimeout reports a stall.
	clock.Advance(61 * time.Second)
	require.True(t, lq.stalled())

	// Fresh progress clears the stall.
	lq.markProgress()
	require.False(t, lq.stalled())
}

func TestLogQueuer_HeartbeatKeepsHealthy(t *testing.T) {
	t.Parallel()

	clock := quartz.NewMock(t)
	lq := &logQueuer{
		clock:             clock,
		heartbeatInterval: 15 * time.Second,
		livenessTimeout:   60 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go lq.work(ctx, done)

	// The work loop records progress at startup. It runs in a goroutine, so poll
	// until it has marked progress rather than racing the immediate check.
	require.Eventually(t, func() bool { return !lq.stalled() }, 2*time.Second, 10*time.Millisecond)

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()

	// A single heartbeat advance keeps the loop reported as healthy, because
	// the heartbeat case refreshes lastProgress before livenessTimeout elapses.
	clock.Advance(15 * time.Second).MustWait(waitCtx)
	require.False(t, lq.stalled())

	// Fire a couple more heartbeats. Each refresh keeps the loop healthy even
	// though real time has advanced well past livenessTimeout.
	clock.Advance(15 * time.Second).MustWait(waitCtx)
	clock.Advance(15 * time.Second).MustWait(waitCtx)
	require.False(t, lq.stalled())

	cancel()
	// Wait for the loop to exit so the mock clock has no dangling ticker.
	<-done
}

func TestLivenessHandler(t *testing.T) {
	t.Parallel()

	t.Run("healthy", func(t *testing.T) {
		t.Parallel()
		handler := livenessHandler(func() bool { return false })
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("stalled", func(t *testing.T) {
		t.Parallel()
		handler := livenessHandler(func() bool { return true })
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})
}
