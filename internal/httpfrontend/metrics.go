package httpfrontend

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func (s *Server) metricsHandler() http.Handler {
	registry := prometheus.NewRegistry()
	resourcesInUse := func() float64 { return float64(s.gateway.ResourcesInUse()) }
	queuedRequests := func() float64 { return float64(s.gateway.QueueLength()) }
	clientOperationsInUse := func() float64 { return float64(len(s.clientOperationSlots)) }
	concurrencyUtilization := func() float64 {
		resourceRatio := resourcesInUse() / float64(s.gateway.MaxResources())
		clientOperationRatio := clientOperationsInUse() / float64(cap(s.clientOperationSlots))
		// The Gateway and HTTP frontend limits are independent, so utilization reports
		// the higher fraction.
		return max(resourceRatio, clientOperationRatio)
	}
	registry.MustRegister(
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "lpgw_concurrency_utilization_ratio",
				Help: "Larger of the Gateway resource utilization and HTTP client-operation utilization. A value of 1 means at least one limit is full.",
			},
			concurrencyUtilization,
		),
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "lpgw_gateway_resources_in_use",
				Help: "Number of Gateway resources currently holding Gateway slots.",
			},
			resourcesInUse,
		),
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "lpgw_queued_requests",
				Help: "Number of Gateway Begin calls waiting in the Gateway queue.",
			},
			queuedRequests,
		),
		prometheus.NewGaugeFunc(
			prometheus.GaugeOpts{
				Name: "lpgw_http_client_operations_in_use",
				Help: "Number of /ws and /scripts operations currently using the HTTP frontend limit.",
			},
			clientOperationsInUse,
		),
	)
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}
