package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	k8smetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	_ "k8s.io/component-base/metrics/prometheus/restclient"
)

const (
	namespace                   = "namespace"
	invalidScalingValueErrors   = "invalid_scaling_value_errors"
	conflictErrors              = "conflict_errors"
	genericErrors               = "generic_errors"
	parsingWorkloadScopeErrors  = "parsing_workload_scope_errors"
	parsingNamespaceScopeErrors = "parsing_namespace_scope_errors"
)

type Metrics struct {
	downscaledWorkloadGauge        *k8smetrics.GaugeVec
	upscaledWorkloadGauge          *k8smetrics.GaugeVec
	scalingErrorWorkloadGauge      *k8smetrics.GaugeVec
	excludedWorkloadGauge          *k8smetrics.GaugeVec
	savedMemoryGauge               *k8smetrics.GaugeVec
	savedCPUGauge                  *k8smetrics.GaugeVec
	downscalerCycleDurationSeconds *k8smetrics.Gauge
	downscalerExecutionsTotal      *k8smetrics.Counter
	lastSuccessfulCycleTimestamp   *k8smetrics.Gauge
	scanningGauge                  *k8smetrics.Gauge
	workloadScanErrorsTotal        *k8smetrics.CounterVec
}

func NewMetrics(dryRun bool) *Metrics {
	return &Metrics{
		downscaledWorkloadGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: metricName("downscaled_workloads", dryRun),
				Help: helperDescription("downscaled workloads managed by kubedownscaler broken down by namespace.", dryRun),
			}, []string{namespace},
		),
		upscaledWorkloadGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: metricName("upscaled_workloads", dryRun),
				Help: helperDescription("upscaled workloads managed by kubedownscaler broken down by namespace.", dryRun),
			}, []string{namespace},
		),
		excludedWorkloadGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: metricName("excluded_workloads", dryRun),
				Help: helperDescription("workloads excluded from kubedownscaler management broken down by namespace.", dryRun),
			}, []string{namespace},
		),
		savedMemoryGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: metricName("current_saved_memory_bytes", dryRun),
				Help: helperDescription("bytes of memory saved by kubedownscaler downscaling actions.", dryRun),
			}, []string{namespace},
		),
		savedCPUGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: metricName("current_saved_cpu_cores", dryRun),
				Help: helperDescription("cores of cpu saved by kubedownscaler downscaling actions.", dryRun),
			}, []string{namespace},
		),

		// always stable (never marked as "potential")
		scalingErrorWorkloadGauge: k8smetrics.NewGaugeVec(
			&k8smetrics.GaugeOpts{
				Name: "kubedownscaler_scaling_errors",
				Help: "Number of scaling errors encountered during the scale process.",
			}, []string{namespace, "type"},
		),
		downscalerCycleDurationSeconds: k8smetrics.NewGauge(
			&k8smetrics.GaugeOpts{
				Name: "kubedownscaler_cycle_duration_seconds",
				Help: "Duration of kubedownscaler cycle in seconds.",
			},
		),
		downscalerExecutionsTotal: k8smetrics.NewCounter(
			&k8smetrics.CounterOpts{
				Name: "kubedownscaler_cycle_executions_total",
				Help: "Number of cycles completed by kubedownscaler since being instantiated.",
			},
		),
		lastSuccessfulCycleTimestamp: k8smetrics.NewGauge(
			&k8smetrics.GaugeOpts{
				Name: "kubedownscaler_last_successful_cycle_timestamp_seconds",
				Help: "Unix time the last kubedownscaler cycle completed.",
			},
		),
		scanningGauge: k8smetrics.NewGauge(
			&k8smetrics.GaugeOpts{
				Name: "kubedownscaler_scanning",
				Help: "1 while this instance runs the scan loop (the leader, or leader election is off), else 0.",
			},
		),
		workloadScanErrorsTotal: k8smetrics.NewCounterVec(
			&k8smetrics.CounterOpts{
				Name: "kubedownscaler_workload_scan_errors_total",
				Help: "Number of workload scans that failed, broken down by workload kind.",
			}, []string{"kind"},
		),
	}
}

func (m *Metrics) RegisterAll() {
	legacyregistry.MustRegister(m.downscaledWorkloadGauge)
	legacyregistry.MustRegister(m.upscaledWorkloadGauge)
	legacyregistry.MustRegister(m.excludedWorkloadGauge)
	legacyregistry.MustRegister(m.savedMemoryGauge)
	legacyregistry.MustRegister(m.savedCPUGauge)
	legacyregistry.MustRegister(m.scalingErrorWorkloadGauge)
	legacyregistry.MustRegister(m.downscalerCycleDurationSeconds)
	legacyregistry.MustRegister(m.downscalerExecutionsTotal)
	legacyregistry.MustRegister(m.lastSuccessfulCycleTimestamp)
	legacyregistry.MustRegister(m.scanningGauge)
	legacyregistry.MustRegister(m.workloadScanErrorsTotal)
}

// SetScanning records whether this instance runs the scan loop. Safe on a nil Metrics.
func (m *Metrics) SetScanning(scanning bool) {
	if m == nil {
		return
	}

	if scanning {
		m.scanningGauge.Set(1)

		return
	}

	m.scanningGauge.Set(0)
}

// IncrementWorkloadScanErrors counts a failed workload scan for the given kind. Safe on a nil Metrics.
func (m *Metrics) IncrementWorkloadScanErrors(kind string) {
	if m == nil {
		return
	}

	m.workloadScanErrorsTotal.WithLabelValues(kind).Inc()
}

func (m *Metrics) UpdateMetrics(
	metricsEnabled bool,
	currentNamespaceToMetrics map[string]*NamespaceMetricsHolder,
	previousNamespacesToMetrics map[string]*NamespaceMetricsHolder,
	cycleDuration float64,
) {
	if !metricsEnabled {
		return
	}

	// delete metrics for namespaces that are no longer present in the cluster
	for previousNamespace := range previousNamespacesToMetrics {
		if _, exists := currentNamespaceToMetrics[previousNamespace]; exists {
			continue
		}

		m.downscaledWorkloadGauge.DeleteLabelValues(previousNamespace)
		m.upscaledWorkloadGauge.DeleteLabelValues(previousNamespace)
		m.scalingErrorWorkloadGauge.DeletePartialMatch(prometheus.Labels{namespace: previousNamespace})
		m.excludedWorkloadGauge.DeleteLabelValues(previousNamespace)
		m.savedMemoryGauge.DeleteLabelValues(previousNamespace)
		m.savedCPUGauge.DeleteLabelValues(previousNamespace)
	}

	// update metrics for current namespaces
	for currentNamespace, metricsRecord := range currentNamespaceToMetrics {
		m.downscaledWorkloadGauge.WithLabelValues(currentNamespace).Set(metricsRecord.DownscaledWorkloads())
		m.upscaledWorkloadGauge.WithLabelValues(currentNamespace).Set(metricsRecord.UpscaledWorkloads())
		m.excludedWorkloadGauge.WithLabelValues(currentNamespace).Set(metricsRecord.ExcludedWorkloads())
		m.scalingErrorWorkloadGauge.WithLabelValues(currentNamespace, invalidScalingValueErrors).Set(metricsRecord.InvalidScalingValueErrors())
		m.scalingErrorWorkloadGauge.WithLabelValues(currentNamespace, conflictErrors).Set(metricsRecord.ConflictErrors())
		m.scalingErrorWorkloadGauge.WithLabelValues(currentNamespace, genericErrors).Set(metricsRecord.GenericErrors())
		m.scalingErrorWorkloadGauge.WithLabelValues(currentNamespace, parsingWorkloadScopeErrors).Set(metricsRecord.ParsingWorkloadScopeErrors())
		m.scalingErrorWorkloadGauge.WithLabelValues(currentNamespace, parsingNamespaceScopeErrors).Set(
			metricsRecord.ParsingNamespaceScopeErrors())
		m.savedMemoryGauge.WithLabelValues(currentNamespace).Set(metricsRecord.SavedMemoryBytes())
		m.savedCPUGauge.WithLabelValues(currentNamespace).Set(metricsRecord.SavedCPUCores())
	}

	m.downscalerCycleDurationSeconds.Set(cycleDuration)
	m.downscalerExecutionsTotal.Inc()
	m.lastSuccessfulCycleTimestamp.Set(float64(time.Now().Unix()))
}
