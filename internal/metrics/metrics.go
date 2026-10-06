package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	requests   *prometheus.CounterVec
	rejections *prometheus.CounterVec
	latency    *prometheus.HistogramVec
}

func New() *Metrics {
	m := &Metrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_requests_total",
			Help: "Total requests by status and tier",
		}, []string{"status", "tier"}),

		rejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gateway_rate_limit_rejections_total",
			Help: "Rate-limit rejections by tier",
		}, []string{"tier"}),

		latency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gateway_request_duration_seconds",
			Help:    "Request latency histogram",
			Buckets: prometheus.DefBuckets,
		}, []string{"tier"}),
	}
	prometheus.MustRegister(m.requests, m.rejections, m.latency)
	return m
}

func (m *Metrics) Handler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) { h.ServeHTTP(c.Writer, c.Request) }
}

func (m *Metrics) RequestMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		status := strconv.Itoa(c.Writer.Status())
		tier := c.GetString("client_tier")
		if tier == "" {
			tier = "anonymous"
		}
		m.requests.WithLabelValues(status, tier).Inc()
		m.latency.WithLabelValues(tier).Observe(time.Since(start).Seconds())
		if c.Writer.Status() == http.StatusTooManyRequests {
			m.rejections.WithLabelValues(tier).Inc()
		}
	}
}
