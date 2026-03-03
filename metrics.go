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

var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coder_logstream_requests_total",
		Help: "Total number of requests to the Coder API.",
	}, []string{"method", "status"})
)

func init() {
	prometheus.MustRegister(requestsTotal)

	// Initialize label combinations so they appear in /metrics at zero.
	for _, method := range []string{"PostLogSource", "ConnectRPC", "SendLog"} {
		requestsTotal.WithLabelValues(method, "success")
		requestsTotal.WithLabelValues(method, "failure")
	}
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}

// record is a helper that increments the appropriate request counter.
func record(method string, err error) {
	if err != nil {
		requestsTotal.WithLabelValues(method, "failure").Inc()
	} else {
		requestsTotal.WithLabelValues(method, "success").Inc()
	}
}

// instrumentedClient wraps agentsdk.Client to record Prometheus metrics
// on every API call. This keeps metric instrumentation in one place
// rather than scattered across call sites.
type instrumentedClient struct {
	*agentsdk.Client
}

func newInstrumentedClient(coderURL *url.URL, token string) *instrumentedClient {
	return &instrumentedClient{
		Client: agentsdk.New(coderURL, agentsdk.WithFixedToken(token)),
	}
}

func (c *instrumentedClient) PostLogSource(ctx context.Context, req agentsdk.PostLogSourceRequest) (codersdk.WorkspaceAgentLogSource, error) {
	resp, err := c.Client.PostLogSource(ctx, req)
	record("PostLogSource", err)
	return resp, err
}

// connectLogDest establishes the appropriate RPC connection based on
// server capabilities, recording metrics for the attempt.
func (c *instrumentedClient) connectLogDest(ctx context.Context, supportsRole bool) (agentsdk.LogDest, drpc.Conn, error) {
	if supportsRole {
		arpc, _, err := c.ConnectRPC28WithRole(ctx, "logstream-kube")
		record("ConnectRPC", err)
		if err != nil {
			return nil, nil, err
		}
		return arpc, arpc.DRPCConn(), nil
	}
	arpc, err := c.ConnectRPC20(ctx)
	record("ConnectRPC", err)
	if err != nil {
		return nil, nil, err
	}
	return arpc, arpc.DRPCConn(), nil
}

// recordSendResult records the result of a log send operation.
func recordSendResult(err error) {
	record("SendLog", err)
}
