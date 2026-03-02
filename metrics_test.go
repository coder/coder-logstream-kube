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

func getCounterValue(t *testing.T, cv *prometheus.CounterVec, label string) float64 {
	t.Helper()
	m := &dto.Metric{}
	c, err := cv.GetMetricWithLabelValues(label)
	require.NoError(t, err)
	require.NoError(t, c.Write(m))
	return m.GetCounter().GetValue()
}

func TestMetricsIncrement(t *testing.T) {
	t.Parallel()

	// Record baseline values (metrics are global and may have been
	// incremented by other tests running in the same process).
	baseSuccess := getCounterValue(t, requestsTotal, "success")
	baseFailure := getCounterValue(t, requestsTotal, "failure")
	baseNetwork := getCounterValue(t, errorsTotal, "network")

	// Simulate success
	requestsTotal.WithLabelValues("success").Inc()
	require.Equal(t, baseSuccess+1, getCounterValue(t, requestsTotal, "success"))

	// Simulate failure
	requestsTotal.WithLabelValues("failure").Inc()
	errorsTotal.WithLabelValues("network").Inc()
	require.Equal(t, baseFailure+1, getCounterValue(t, requestsTotal, "failure"))
	require.Equal(t, baseNetwork+1, getCounterValue(t, errorsTotal, "network"))
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
	requestsTotal.WithLabelValues("success").Inc()

	resp, err := http.Get("http://" + addr + "/metrics")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.True(t, strings.Contains(string(body), "coder_logstream_requests_total"),
		"expected coder_logstream_requests_total in metrics output")
	require.True(t, strings.Contains(string(body), "coder_logstream_errors_total"),
		"expected coder_logstream_errors_total in metrics output")
}
