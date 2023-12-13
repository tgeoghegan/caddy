package caddyhttp

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/caddyserver/caddy/v2/internal/metrics"
)

// Metrics configures metrics observations.
// EXPERIMENTAL and subject to change or removal.
type Metrics struct{}

var httpMetrics = struct {
	init             sync.Once
	requestInFlight  *prometheus.GaugeVec
	requestCount     *prometheus.CounterVec
	requestErrors    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	requestSize      *prometheus.HistogramVec
	responseSize     *prometheus.HistogramVec
	responseDuration *prometheus.HistogramVec
}{
	init: sync.Once{},
}

func initHTTPMetrics() {
	const ns, sub = "caddy", "http"

	basicLabels := []string{"server", "handler"}
	httpMetrics.requestInFlight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "requests_in_flight",
		Help:      "Number of requests currently handled by this server.",
	}, basicLabels)
	httpMetrics.requestErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "request_errors_total",
		Help:      "Number of requests resulting in middleware errors.",
	}, basicLabels)
	httpMetrics.requestCount = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "requests_total",
		Help:      "Counter of HTTP(S) requests made.",
	}, basicLabels)

	// TODO: allow these to be customized in the config
	durationBuckets := prometheus.DefBuckets
	sizeBuckets := prometheus.ExponentialBuckets(256, 4, 8)

	httpLabels := []string{"server", "handler", "code", "method"}
	httpMetrics.requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "request_duration_seconds",
		Help:      "Histogram of round-trip request durations.",
		Buckets:   durationBuckets,
	}, httpLabels)
	httpMetrics.requestSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "request_size_bytes",
		Help:      "Total size of the request. Includes body",
		Buckets:   sizeBuckets,
	}, httpLabels)
	httpMetrics.responseSize = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "response_size_bytes",
		Help:      "Size of the returned response.",
		Buckets:   sizeBuckets,
	}, httpLabels)
	httpMetrics.responseDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: ns,
		Subsystem: sub,
		Name:      "response_duration_seconds",
		Help:      "Histogram of times to first byte in response bodies.",
		Buckets:   durationBuckets,
	}, httpLabels)
}

// serverNameFromContext extracts the current server name from the context.
// Returns "UNKNOWN" if none is available (should probably never happen).
func serverNameFromContext(ctx context.Context) string {
	srv, ok := ctx.Value(ServerCtxKey).(*Server)
	if !ok || srv == nil || srv.name == "" {
		return "UNKNOWN"
	}
	return srv.name
}

type metricsInstrumentedHandler struct {
	handler string
	mh      MiddlewareHandler
}

func newMetricsInstrumentedHandler(handler string, mh MiddlewareHandler) *metricsInstrumentedHandler {
	httpMetrics.init.Do(func() {
		fmt.Printf("initializing HTTP metrics")
		initHTTPMetrics()
	})

	return &metricsInstrumentedHandler{handler, mh}
}

func (h *metricsInstrumentedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request, next Handler) error {
	server := serverNameFromContext(r.Context())
	labels := prometheus.Labels{"server": server, "handler": h.handler}
	method := metrics.SanitizeMethod(r.Method)
	// the "code" value is set later, but initialized here to eliminate the possibility
	// of a panic
	statusLabels := prometheus.Labels{"server": server, "handler": h.handler, "method": method, "code": ""}

	inFlight := httpMetrics.requestInFlight.With(labels)
	inFlight.Inc()
	defer inFlight.Dec()

	start := time.Now()

	// This is a _bit_ of a hack - it depends on the ShouldBufferFunc always
	// being called when the headers are written.
	// Effectively the same behaviour as promhttp.InstrumentHandlerTimeToWriteHeader.
	writeHeaderRecorder := ShouldBufferFunc(func(status int, header http.Header) bool {
		statusLabels["code"] = metrics.SanitizeCode(status)
		ttfb := time.Since(start).Seconds()
		httpMetrics.responseDuration.With(statusLabels).Observe(ttfb)
		return false
	})
	wrec := NewResponseRecorder(w, nil, writeHeaderRecorder)
	err := h.mh.ServeHTTP(wrec, r, next)
	dur := time.Since(start).Seconds()
	httpMetrics.requestCount.With(labels).Inc()

	observeRequest := func(status int) {
		requestSize := float64(computeApproximateRequestSize(r))
		responseSize := float64(wrec.Size())
		fmt.Printf("incrementing metrics with %d %v %v into %p\n", status, requestSize, responseSize, &httpMetrics)
		// If the code hasn't been set yet, and we didn't encounter an error, we're
		// probably falling through with an empty handler.
		if statusLabels["code"] == "" {
			// we still sanitize it, even though it's likely to be 0. A 200 is
			// returned on fallthrough so we want to reflect that.
			statusLabels["code"] = metrics.SanitizeCode(status)
		}
		fmt.Printf("incrementing metrics with labels %v into %p\n", statusLabels, &httpMetrics)

		httpMetrics.requestDuration.With(statusLabels).Observe(dur)
		httpMetrics.requestSize.With(statusLabels).Observe(requestSize)
		httpMetrics.responseSize.With(statusLabels).Observe(responseSize)
	}

	if err != nil {
		fmt.Printf("error %v\n", err)
		if handlerError, ok := err.(HandlerError); ok {
			fmt.Printf("error is a handler error\n")
			observeRequest(handlerError.StatusCode)
		} else {
			fmt.Printf("error is not a handler error\n")
		}

		httpMetrics.requestErrors.With(labels).Inc()
		return err
	}

	observeRequest(wrec.Status())

	return nil
}

// taken from https://github.com/prometheus/client_golang/blob/6007b2b5cae01203111de55f753e76d8dac1f529/prometheus/promhttp/instrument_server.go#L298
func computeApproximateRequestSize(r *http.Request) int {
	s := 0
	if r.URL != nil {
		s += len(r.URL.String())
	}

	s += len(r.Method)
	s += len(r.Proto)
	for name, values := range r.Header {
		s += len(name)
		for _, value := range values {
			s += len(value)
		}
	}
	s += len(r.Host)

	// N.B. r.Form and r.MultipartForm are assumed to be included in r.URL.

	if r.ContentLength != -1 {
		s += int(r.ContentLength)
	}
	return s
}
