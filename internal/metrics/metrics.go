package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	RepoRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_gw_repo_requests_total",
		Help: "Total number of repository operations.",
	}, []string{"method", "status"})

	RepoRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sms_gw_repo_request_duration_seconds",
		Help:    "Repository operation latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method"})

	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_gw_http_requests_total",
		Help: "Total number of HTTP requests.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sms_gw_http_request_duration_seconds",
		Help:    "HTTP request latency in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	MessagesDeliveredTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "sms_gw_messages_delivered_total",
		Help: "Total number of message delivery outcomes.",
	}, []string{"outcome"})
)

func Handler() http.Handler {
	return promhttp.Handler()
}

type Classifier func(error) bool

func ObserveRepo(method string, errp *error, isRejection Classifier, start time.Time) {
	status := "success"
	if errp != nil && *errp != nil {
		if isRejection != nil && isRejection(*errp) {
			status = "rejected"
		} else {
			status = "error"
		}
	}
	RepoRequestsTotal.WithLabelValues(method, status).Inc()
	RepoRequestDuration.WithLabelValues(method).Observe(time.Since(start).Seconds())
}

func ObserveHTTP(method, pattern string, statusCode int, start time.Time) {
	if pattern == "" {
		pattern = "unmatched"
	}
	HTTPRequestsTotal.WithLabelValues(method, pattern, strconv.Itoa(statusCode)).Inc()
	HTTPRequestDuration.WithLabelValues(method, pattern).Observe(time.Since(start).Seconds())
}

func RecordMessageOutcome(outcome string) {
	MessagesDeliveredTotal.WithLabelValues(outcome).Inc()
}

func RecordRescued(n int64) {
	if n > 0 {
		MessagesDeliveredTotal.WithLabelValues("rescued").Add(float64(n))
	}
}
