package main

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coder_logstream_requests_total",
		Help: "Total number of requests to the Coder API.",
	}, []string{"status"}) // "success" | "failure"

	errorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "coder_logstream_errors_total",
		Help: "Total number of errors by type.",
	}, []string{"type"}) // "auth" | "network" | "parse"
)

func init() {
	prometheus.MustRegister(requestsTotal, errorsTotal)

	// Initialize all label combinations so they appear in /metrics output
	// even before the first increment.
	requestsTotal.WithLabelValues("success")
	requestsTotal.WithLabelValues("failure")
	errorsTotal.WithLabelValues("network")
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}
