package metrics

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
)

var (
	// RequestTotal 请求总数
	RequestTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_gateway_requests_total",
			Help: "Total number of requests",
		},
		[]string{"method", "path", "status", "model"},
	)

	// RequestDuration 请求耗时
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_gateway_request_duration_seconds",
			Help:    "Request duration in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method", "path", "model"},
	)

	// UpstreamLatency 上游延迟
	UpstreamLatency = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "llm_gateway_upstream_latency_seconds",
			Help:    "Upstream request latency in seconds",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"provider", "model"},
	)

	// TokenUsage token 用量
	TokenUsage = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "llm_gateway_token_usage_total",
			Help: "Total token usage",
		},
		[]string{"model", "type"}, // type: prompt | completion
	)

	// CircuitBreakerState 熔断器状态
	CircuitBreakerState = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "llm_gateway_circuit_breaker_state",
			Help: "Circuit breaker state (0=closed, 1=open, 2=half-open)",
		},
		[]string{"provider", "model"},
	)

	// RateLimitHits 限流命中数
	RateLimitHits = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "llm_gateway_rate_limit_hits_total",
			Help: "Total number of rate limit hits",
		},
	)
)

func init() {
	prometheus.MustRegister(
		RequestTotal,
		RequestDuration,
		UpstreamLatency,
		TokenUsage,
		CircuitBreakerState,
		RateLimitHits,
	)
}

// RecordRequest 记录请求级别指标 (RequestTotal + RequestDuration)
func RecordRequest(method, path string, status int, model string, duration float64) {
	RequestTotal.WithLabelValues(method, path, fmt.Sprintf("%d", status), model).Inc()
	RequestDuration.WithLabelValues(method, path, model).Observe(duration)
}

// RecordTokenUsage 记录 token 用量指标
func RecordTokenUsage(model string, input, output int) {
	TokenUsage.WithLabelValues(model, "input").Add(float64(input))
	TokenUsage.WithLabelValues(model, "output").Add(float64(output))
}

// Handler 返回 Prometheus 格式的 metrics handler（兼容原有行为）
func Handler() gin.HandlerFunc {
	h := promhttp.Handler()
	return func(c *gin.Context) {
		h.ServeHTTP(c.Writer, c.Request)
	}
}

// JSONHandler 返回 JSON 格式的 metrics handler
func JSONHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}

		result := exportToJSON(families)
		c.JSON(200, result)
	}
}

// MetricFamily 单个 metric family 的结构
type MetricFamily struct {
	Name   string      `json:"name"`
	Help   string      `json:"help"`
	Type   string      `json:"type"`
	Metrics []MetricItem `json:"metrics"`
}

// MetricItem 单个指标项
type MetricItem struct {
	Labels map[string]string `json:"labels,omitempty"`
	Value  json.Number      `json:"value"`
}

// MetricsMap 顶层导出结构
type MetricsMap struct {
	Info     map[string]interface{} `json:"info,omitempty"`
	Counters []MetricFamily         `json:"counters"`
	Gauges   []MetricFamily         `json:"gauges"`
	Histograms []MetricFamily       `json:"histograms"`
}

func exportToJSON(families []*dto.MetricFamily) MetricsMap {
	result := MetricsMap{
		Counters:   []MetricFamily{},
		Gauges:     []MetricFamily{},
		Histograms: []MetricFamily{},
	}

	// Go 运行时指标归入 info
	info := map[string]interface{}{}

	for _, family := range families {
		mf := MetricFamily{
			Name:    family.GetName(),
			Help:    family.GetHelp(),
			Type:    dto.MetricType_name[int32(family.GetType())],
			Metrics: []MetricItem{},
		}

		for _, metric := range family.Metric {
			item := MetricItem{Labels: map[string]string{}}

			// 提取 labels
			for _, lp := range metric.Label {
				if lp.GetName() == "__name__" {
					continue
				}
				item.Labels[lp.GetName()] = lp.GetValue()
			}

			switch family.GetType() {
			case dto.MetricType_COUNTER:
				item.Value = json.Number(fmt.Sprintf("%g", metric.GetCounter().GetValue()))
			case dto.MetricType_GAUGE:
				item.Value = json.Number(fmt.Sprintf("%g", metric.GetGauge().GetValue()))
			case dto.MetricType_HISTOGRAM:
				h := metric.GetHistogram()
				// bucket 用 le label
				for _, b := range h.Bucket {
					bucketItem := MetricItem{
						Labels: map[string]string{
							"le":    fmt.Sprintf("%g", b.GetUpperBound()),
						},
						Value: json.Number(fmt.Sprintf("%d", b.GetCumulativeCount())),
					}
					mf.Metrics = append(mf.Metrics, bucketItem)
				}
				// _sum / _count 作为单独指标项
				mf.Metrics = append(mf.Metrics, MetricItem{
					Labels: map[string]string{"__name__": family.GetName() + "_sum"},
					Value:  json.Number(fmt.Sprintf("%g", h.GetSampleSum())),
				})
				mf.Metrics = append(mf.Metrics, MetricItem{
					Labels: map[string]string{"__name__": family.GetName() + "_count"},
					Value:  json.Number(fmt.Sprintf("%d", h.GetSampleCount())),
				})
			case dto.MetricType_SUMMARY:
				for _, q := range metric.Summary.Quantile {
					mf.Metrics = append(mf.Metrics, MetricItem{
						Labels: map[string]string{
							"quantile": fmt.Sprintf("%g", q.GetQuantile()),
						},
						Value: json.Number(fmt.Sprintf("%g", q.GetValue())),
					})
				}
			}

			// Go 运行时指标归入 info
			if strings.HasPrefix(family.GetName(), "go_") || strings.HasPrefix(family.GetName(), "promhttp_") {
				info[family.GetName()] = toItemValue(metric, family.GetType())
				continue
			}

			mf.Metrics = append(mf.Metrics, item)
		}

		switch family.GetType() {
		case dto.MetricType_COUNTER:
			result.Counters = append(result.Counters, mf)
		case dto.MetricType_GAUGE:
			result.Gauges = append(result.Gauges, mf)
		case dto.MetricType_HISTOGRAM, dto.MetricType_SUMMARY:
			result.Histograms = append(result.Histograms, mf)
		}
	}

	if len(info) > 0 {
		result.Info = info
	}

	return result
}

func toItemValue(metric *dto.Metric, mtype dto.MetricType) interface{} {
	switch mtype {
	case dto.MetricType_GAUGE:
		return metric.GetGauge().GetValue()
	case dto.MetricType_COUNTER:
		return metric.GetCounter().GetValue()
	}
	return nil
}

// GatheredMap 将所有指标拍平为 map[name]map[labels]value 结构，方便解释器使用
func GatheredMap() map[string]map[string]interface{} {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return nil
	}

	result := map[string]map[string]interface{}{}

	for _, family := range families {
		if strings.HasPrefix(family.GetName(), "go_") || strings.HasPrefix(family.GetName(), "promhttp_") {
			continue // 跳过运行时指标
		}

		items := map[string]interface{}{}
		for _, metric := range family.Metric {
			var labels []string
			for _, lp := range metric.Label {
				if lp.GetName() == "__name__" {
					continue
				}
				labels = append(labels, lp.GetName()+"="+lp.GetValue())
			}
			key := strings.Join(labels, ",")

			switch family.GetType() {
			case dto.MetricType_COUNTER:
				items[key] = metric.GetCounter().GetValue()
			case dto.MetricType_GAUGE:
				items[key] = metric.GetGauge().GetValue()
			case dto.MetricType_HISTOGRAM:
				h := metric.GetHistogram()
				buckets := []interface{}{}
				for _, b := range h.Bucket {
					buckets = append(buckets, map[string]interface{}{
						"le":    fmt.Sprintf("%g", b.GetUpperBound()),
						"count": json.Number(fmt.Sprintf("%d", b.GetCumulativeCount())),
					})
				}
				sort.Slice(buckets, func(i, j int) bool {
					return buckets[i].(map[string]interface{})["le"].(string) < buckets[j].(map[string]interface{})["le"].(string)
				})
				items[key] = map[string]interface{}{
					"buckets": buckets,
					"sum":     h.GetSampleSum(),
					"count":   h.GetSampleCount(),
				}
			}
		}
		result[family.GetName()] = items
	}

	return result
}
