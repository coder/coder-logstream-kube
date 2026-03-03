package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/require"
)

func getCounterValue(t *testing.T, cv *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := cv.GetMetricWithLabelValues(labels...)
	require.NoError(t, err)
	require.NoError(t, c.Write(m))
	return m.GetCounter().GetValue()
}

func TestMetricsIncrement(t *testing.T) {
	t.Parallel()

	// Record baseline values (metrics are global and may have been
	// incremented by other tests running in the same process).
	baseSuccess := getCounterValue(t, requestsTotal, "PostLogSource", "success")
	baseFailure := getCounterValue(t, requestsTotal, "PostLogSource", "failure")
	baseSendSuccess := getCounterValue(t, requestsTotal, "SendLog", "success")

	// Simulate success via record helper
	record("PostLogSource", nil)
	require.Equal(t, baseSuccess+1, getCounterValue(t, requestsTotal, "PostLogSource", "success"))

	// Simulate failure via record helper
	record("PostLogSource", io.ErrUnexpectedEOF)
	require.Equal(t, baseFailure+1, getCounterValue(t, requestsTotal, "PostLogSource", "failure"))

	// Simulate send success
	recordSendResult(nil)
	require.Equal(t, baseSendSuccess+1, getCounterValue(t, requestsTotal, "SendLog", "success"))
}

func TestMetricsHandler(t *testing.T) {
	t.Parallel()

	handler := metricsHandler()
	require.NotNil(t, handler)
}

func TestMetricsEndpoint(t *testing.T) {
	t.Parallel()

	// Pick a random free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := listener.Addr().String()
	_ = listener.Close()

	mux := http.NewServeMux()
	mux.Handle("/metrics", metricsHandler())
	srv := &http.Server{Addr: addr, Handler: mux}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() { _ = srv.Close() })

	// Wait for the server to be ready.
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + addr + "/metrics")
		if err != nil {
			return false
		}
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 2*time.Second, 50*time.Millisecond)

	// Bump a counter and verify it appears in the output.
	record("PostLogSource", nil)

	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.True(t, strings.Contains(string(body), "coder_logstream_requests_total"),
		"expected coder_logstream_requests_total in metrics output")
}
