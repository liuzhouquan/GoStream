package platform

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	registry      *prometheus.Registry
	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	outbox        *prometheus.CounterVec
	feedCache     *prometheus.CounterVec
	wsConnections prometheus.Gauge
}

func newServerMetrics() *serverMetrics {
	m := &serverMetrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_http_requests_total", Help: "Completed HTTP requests by method, route and status.",
		}, []string{"method", "route", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "gostream_http_request_duration_seconds", Help: "HTTP request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
		outbox: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_outbox_events_total", Help: "Outbox event dispatch attempts by result.",
		}, []string{"result"}),
		feedCache: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_feed_cache_total", Help: "Anonymous Feed cache outcomes.",
		}, []string{"result"}),
		wsConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gostream_websocket_connections", Help: "Current local WebSocket connections.",
		}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.outbox, m.feedCache, m.wsConnections)
	m.registry.MustRegister(prometheus.NewGoCollector(), prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))
	return m
}

func (m *serverMetrics) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		m.requests.WithLabelValues(c.Request.Method, route, strconv.Itoa(c.Writer.Status())).Inc()
		m.duration.WithLabelValues(c.Request.Method, route).Observe(time.Since(start).Seconds())
	}
}

func (m *serverMetrics) handler() gin.HandlerFunc {
	return gin.WrapH(promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
}
