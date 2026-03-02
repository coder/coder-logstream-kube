package main

import (
	"testing"

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
