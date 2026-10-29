package platform

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	registry        *prometheus.Registry
	requests        *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	outbox          *prometheus.CounterVec
	brokerMessages  *prometheus.CounterVec
	brokerConnected *prometheus.GaugeVec
	feedCache       *prometheus.CounterVec
	rateLimit       *prometheus.CounterVec
	feedFanout      *prometheus.CounterVec
	feedFanoutAge   prometheus.Histogram
	feedFanoutJobs  prometheus.Gauge
	mediaReconcile  *prometheus.CounterVec
	wsConnections   prometheus.Gauge
}

func newServerMetrics() *serverMetrics {
	m := &serverMetrics{
		registry:        prometheus.NewRegistry(),
		brokerMessages:  prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gostream_rabbitmq_messages_total", Help: "RabbitMQ notification transport outcomes."}, []string{"result"}),
		brokerConnected: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gostream_rabbitmq_connected", Help: "Last observed RabbitMQ connection state by role."}, []string{"role"}),
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
			Name: "gostream_feed_cache_total", Help: "Feed page cache outcomes.",
		}, []string{"result"}),
		rateLimit: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_rate_limit_total", Help: "Public route rate budget outcomes, without client identifiers.",
		}, []string{"route", "result"}),
		feedFanout: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_feed_fanout_batches_total", Help: "Feed fanout batch attempts by result.",
		}, []string{"result"}),
		feedFanoutAge: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "gostream_feed_fanout_completion_seconds", Help: "Elapsed time from durable job creation to fanout completion.",
			Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
		}),
		feedFanoutJobs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gostream_feed_fanout_pending_jobs", Help: "Unfinished fanout jobs at the last worker sample.",
		}),
		mediaReconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gostream_media_reconciliation_total", Help: "Removed orphan files and unused registered assets, or reconciliation errors.",
		}, []string{"result"}),
		wsConnections: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "gostream_websocket_connections", Help: "Current local WebSocket connections.",
		}),
	}
	m.registry.MustRegister(m.requests, m.duration, m.outbox, m.feedCache, m.rateLimit, m.feedFanout, m.feedFanoutAge, m.feedFanoutJobs, m.mediaReconcile, m.wsConnections)
	m.registry.MustRegister(m.brokerMessages, m.brokerConnected)
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
