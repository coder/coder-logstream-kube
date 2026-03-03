package main

import (
	"context"
	"net/http"
	"net/url"

	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/agentsdk"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"storj.io/drpc"
)

// requestMethod identifies the API method being called.
type requestMethod string

const (
	methodPostLogSource requestMethod = "PostLogSource"
	methodConnectRPC    requestMethod = "ConnectRPC"
	methodSendLog       requestMethod = "SendLog"
)

// allMethods is used to pre-initialize all label combinations.
var allMethods = []requestMethod{methodPostLogSource, methodConnectRPC, methodSendLog}

// metricsCollector holds Prometheus metrics for the application.
// It uses a custom registry so metrics are not global, making
// tests deterministic and avoiding flakes from parallel execution.
type metricsCollector struct {
	registry      *prometheus.Registry
	requestsTotal *prometheus.CounterVec
}

func newMetricsCollector() *metricsCollector {
	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coder_logstream_requests_total",
		Help: "Total number of requests to the Coder API.",
	}, []string{"method", "status"})

	registry := prometheus.NewRegistry()
	registry.MustRegister(requestsTotal)

	// Initialize all label combinations so they appear in /metrics at zero.
	for _, method := range allMethods {
		requestsTotal.WithLabelValues(string(method), "success")
		requestsTotal.WithLabelValues(string(method), "failure")
	}

	return &metricsCollector{
		registry:      registry,
		requestsTotal: requestsTotal,
	}
}

func (m *metricsCollector) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// record increments the appropriate request counter.
func (m *metricsCollector) record(method requestMethod, err error) {
	if err != nil {
		m.requestsTotal.WithLabelValues(string(method), "failure").Inc()
	} else {
		m.requestsTotal.WithLabelValues(string(method), "success").Inc()
	}
}

// instrumentedClient wraps agentsdk.Client to record Prometheus metrics
// on every API call. This keeps metric instrumentation in one place
// rather than scattered across call sites.
type instrumentedClient struct {
	*agentsdk.Client
	metrics *metricsCollector
}

func newInstrumentedClient(coderURL *url.URL, token string, metrics *metricsCollector) *instrumentedClient {
	return &instrumentedClient{
		Client:  agentsdk.New(coderURL, agentsdk.WithFixedToken(token)),
		metrics: metrics,
	}
}

func (c *instrumentedClient) PostLogSource(ctx context.Context, req agentsdk.PostLogSourceRequest) (codersdk.WorkspaceAgentLogSource, error) {
	resp, err := c.Client.PostLogSource(ctx, req)
	c.metrics.record(methodPostLogSource, err)
	return resp, err
}

// connectLogDest establishes the appropriate RPC connection based on
// server capabilities, recording metrics for the attempt.
func (c *instrumentedClient) connectLogDest(ctx context.Context, supportsRole bool) (agentsdk.LogDest, drpc.Conn, error) {
	if supportsRole {
		arpc, _, err := c.ConnectRPC28WithRole(ctx, "logstream-kube")
		c.metrics.record(methodConnectRPC, err)
		if err != nil {
			return nil, nil, err
		}
		return arpc, arpc.DRPCConn(), nil
	}
	arpc, err := c.ConnectRPC20(ctx)
	c.metrics.record(methodConnectRPC, err)
	if err != nil {
		return nil, nil, err
	}
	return arpc, arpc.DRPCConn(), nil
}
