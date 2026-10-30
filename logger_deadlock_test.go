package main

import (
	"context"
	"testing"
	"time"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/coder/quartz"
)

// TestLogQueuerRetryFiresWithoutHoldingLock is a regression test for CORE-793.
//
// scheduleRetry arms a timer whose callback enqueues a retry onto l.q. The only
// consumer of l.q is the work loop (work / processLog / processDelete), and it
// needs l.mu to drain the queue. If the callback performs the channel send while
// still holding l.mu, a full queue becomes a lock/channel cycle: the callback
// blocks on the send holding the lock, so the work loop can never drain the queue
// and the logQueuer deadlocks permanently.
//
// The test arms a retry, fills the queue to capacity, and fires the retry timer.
// It then asserts that l.mu is acquirable, which only holds if the callback
// released the lock before the blocking send. The mock clock runs the fired
// callback in a background goroutine, so the test lets that goroutine run and
// then probes the lock with a bounded blocking acquisition. On a regression the
// lock is held forever and the probe fails on timeout instead of hanging.
func TestLogQueuerRetryFiresWithoutHoldingLock(t *testing.T) {
	t.Parallel()

	clock := quartz.NewMock(t)
	logger := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})

	const queueCap = 1
	ch := make(chan agentLog, queueCap)
	lq := &logQueuer{
		metrics:    newMetricsCollector(),
		logger:     logger,
		clock:      clock,
		q:          ch,
		loggers:    map[string]agentLoggerLifecycle{},
		logCache:   logCache{logs: map[string][]agentsdk.Log{}},
		retries:    make(map[string]*retryState),
		maxRetries: 100,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	token := "retry-token"
	lq.scheduleRetry(ctx, token)

	// Fill the queue to capacity so the retry callback's send blocks when it fires.
	for len(ch) < queueCap {
		ch <- agentLog{op: opLog, resourceName: "filler", agentToken: "filler-token"}
	}

	// Fire the retry timer while the queue is full. The callback runs in a background
	// goroutine spawned by the mock clock; give it a moment to run, then probe the
	// lock with a bounded acquisition.
	clock.Advance(time.Second)
	time.Sleep(500 * time.Millisecond)

	locked := make(chan struct{})
	go func() {
		lq.mu.Lock()
		lq.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("retry callback held logQueuer.mu across a full-queue send; the work loop can never drain l.q")
	}
}
