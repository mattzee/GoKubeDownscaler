package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/caas-team/gokubedownscaler/internal/api/kubernetes"
	"github.com/caas-team/gokubedownscaler/internal/pkg/health"
	"github.com/caas-team/gokubedownscaler/internal/pkg/metrics"
	"github.com/caas-team/gokubedownscaler/internal/pkg/scalable"
	"github.com/caas-team/gokubedownscaler/internal/pkg/tracing"
	"github.com/caas-team/gokubedownscaler/internal/pkg/values"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apiserver/pkg/registry/generic/registry"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/component-base/metrics/legacyregistry"
)

const (
	leaseName = "downscaler-lease"
	// legacyHealthAddr and legacyMetricsAddr are used when --port is not set.
	legacyHealthAddr  = ":8081"
	legacyMetricsAddr = ":8085"
	// tracingShutdownTimeout bounds the final span flush on exit.
	tracingShutdownTimeout = 5 * time.Second
)

func main() {
	config, scopeDefault, scopeCli, scopeEnv := initComponent()

	slog.Info(
		"started downscaler",
		"config", config.String(),
		"cliScope", fmt.Sprintf("%+v", scopeCli),
		"envScope", fmt.Sprintf("%+v", scopeEnv),
	)

	slog.Debug("getting client for kubernetes")

	client, err := kubernetes.NewClient(config.Kubeconfig, config.DryRun, config.Qps, config.Burst, config.Timeout)
	if err != nil {
		slog.Error("failed to create new Kubernetes client", "error", err)
		os.Exit(1)
	}

	shutdownTracing, err := tracing.Setup(context.Background(), config.Tracing)
	if err != nil {
		slog.Error("failed to set up tracing", "error", err)
		os.Exit(1)
	}

	defer flushTracing(shutdownTracing)

	// SIGTERM cancels the context, so the scan loop stops between cycles and buffered spans are flushed.
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	defer cancel()

	tracker := health.NewTracker(health.StaleAfter(config.HealthStaleAfter, config.Interval))
	downscalerMetrics := initMetrics(config)

	go serve(config, tracker)

	if !config.LeaderElection {
		runWithoutLeaderElection(client, ctx, scopeDefault, scopeCli, scopeEnv, config, downscalerMetrics, tracker)
		return
	}

	runWithLeaderElection(client, cancel, ctx, scopeDefault, scopeCli, scopeEnv, config, downscalerMetrics, tracker)
}

// flushTracing exports any buffered spans before the process exits.
func flushTracing(shutdown tracing.Shutdown) {
	ctx, cancel := context.WithTimeout(context.Background(), tracingShutdownTimeout)
	defer cancel()

	if err := shutdown(ctx); err != nil {
		slog.Warn("failed to flush traces", "error", err)
	}
}

// serve starts the HTTP listeners for the probes and metrics. With --port set,
// /healthz, /readyz and /metrics share that one listener; otherwise the probes
// are served on :8081 and metrics on :8085.
func serve(config *runtimeConfiguration, tracker *health.Tracker) {
	probes := http.NewServeMux()
	probes.Handle("/healthz", tracker.LivenessHandler())
	probes.Handle("/readyz", tracker.ReadinessHandler())

	if config.Port > 0 {
		if config.MetricsEnabled {
			probes.Handle("/metrics", legacyregistry.Handler())
		}

		listen(net.JoinHostPort("", strconv.Itoa(config.Port)), probes, "probes and metrics")

		return
	}

	if config.MetricsEnabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle("/metrics", legacyregistry.Handler())

		go listen(legacyMetricsAddr, metricsMux, "metrics")
	}

	listen(legacyHealthAddr, probes, "probes")
}

// listen serves handler on addr and exits the process if the listener fails.
func listen(addr string, handler http.Handler, endpoints string) {
	server := &http.Server{
		Addr:         addr,
		Handler:      handler,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("serving http", "endpoints", endpoints, "address", addr)

	err := server.ListenAndServe()
	if err != nil {
		slog.Error("failed to start http server", "endpoints", endpoints, "address", addr, "error", err)
		os.Exit(1)
	}
}

// runWithLeaderElection runs the downscaler with leader election enabled.
func runWithLeaderElection(
	client kubernetes.Client,
	cancel context.CancelFunc,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	config *runtimeConfiguration,
	downscalerMetrics *metrics.Metrics,
	tracker *health.Tracker,
) {
	lease, err := client.CreateLease(leaseName)
	if err != nil {
		slog.Error("failed to create lease", "error", err)
		os.Exit(1)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigs
		cancel()
	}()

	leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
		Lock:            lease,
		ReleaseOnCancel: true,
		LeaseDuration:   30 * time.Second,
		RenewDeadline:   20 * time.Second,
		RetryPeriod:     5 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				slog.Info("started leading")

				err = startScanning(client, ctx, scopeDefault, scopeCli, scopeEnv, config, downscalerMetrics, tracker)
				if err != nil {
					slog.Error("an error occurred while scanning workloads", "error", err)
					cancel()
				}
			},
			OnStoppedLeading: func() {
				slog.Info("stopped leading")
				cancel()
			},
			OnNewLeader: func(identity string) {
				slog.Info("new leader elected", "identity", identity)
			},
		},
	})
}

// runWithoutLeaderElection runs the downscaler without leader election enabled.
func runWithoutLeaderElection(
	client kubernetes.Client,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	config *runtimeConfiguration,
	downscalerMetrics *metrics.Metrics,
	tracker *health.Tracker,
) {
	slog.Warn("proceeding without leader election; this could cause errors when running with multiple replicas")

	err := startScanning(client, ctx, scopeDefault, scopeCli, scopeEnv, config, downscalerMetrics, tracker)
	if err != nil {
		slog.Error("an error occurred while scanning workloads, exiting", "error", err)
		os.Exit(1)
	}
}

// startScanning periodically triggers a scan on all workloads until ctx is canceled.
func startScanning(
	client kubernetes.Client,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	config *runtimeConfiguration,
	downscalerMetrics *metrics.Metrics,
	tracker *health.Tracker,
) error {
	slog.Info("started downscaler scanning process")

	tracker.StartedScanning()
	downscalerMetrics.SetScanning(true)

	defer func() {
		tracker.StoppedScanning()
		downscalerMetrics.SetScanning(false)
	}()

	previousNamespacesToMetrics := newNamespaceToMetrics(config)

	for {
		currentNamespaceToMetrics, err := runCycle(
			client, ctx, scopeDefault, scopeCli, scopeEnv, config, downscalerMetrics, previousNamespacesToMetrics,
		)
		if err != nil {
			if ctx.Err() != nil {
				slog.Info("stopped scanning", "reason", context.Cause(ctx))
				return nil
			}

			return err
		}

		tracker.CycleCompleted()

		previousNamespacesToMetrics = currentNamespaceToMetrics

		if config.Once {
			slog.Debug("once is set to true, exiting")
			return nil
		}

		slog.Debug("waiting until next scan", "interval", config.Interval.String())

		select {
		case <-ctx.Done():
			slog.Info("stopped scanning", "reason", context.Cause(ctx))
			return nil
		case <-time.After(config.Interval):
		}
	}
}

// runCycle scans every workload in scope once and returns the cycle's per-namespace metrics.
func runCycle(
	client kubernetes.Client,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	config *runtimeConfiguration,
	downscalerMetrics *metrics.Metrics,
	previousNamespacesToMetrics map[string]*metrics.NamespaceMetricsHolder,
) (map[string]*metrics.NamespaceMetricsHolder, error) {
	start := time.Now()

	ctx, span := tracing.Tracer().Start(ctx, "downscaler.cycle")
	defer span.End()

	currentNamespaceToMetrics := newNamespaceToMetrics(config)

	workloads, err := client.GetWorkloads(config.IncludeNamespaces, config.IncludeResources, ctx)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to get workloads")

		return nil, fmt.Errorf("failed to get workloads: %w", err)
	}

	workloads = scalable.FilterExcluded(
		workloads,
		config.IncludeLabels,
		config.ExcludeNamespaces,
		config.ExcludeWorkloads,
		currentNamespaceToMetrics,
		slog.Default(),
	)
	span.SetAttributes(attribute.Int("downscaler.workloads", len(workloads)))
	slog.Debug("scanning over workloads matching filters", "amount", len(workloads))

	namespaceScopes, errs := client.GetNamespacesScopes(workloads, ctx)
	if len(errs) > 0 {
		handleNamespaceScopeParsingErrors(errs, config, currentNamespaceToMetrics)
	}

	var waitGroup sync.WaitGroup
	for _, workload := range workloads {
		waitGroup.Add(1)

		go func(workload scalable.Workload) {
			defer waitGroup.Done()

			processWorkload(
				workload, client, ctx, scopeDefault, scopeCli, scopeEnv, namespaceScopes, currentNamespaceToMetrics, config, downscalerMetrics,
			)
		}(workload)
	}

	waitGroup.Wait()
	slog.Debug("successfully scanned all workloads in target")

	downscalerMetrics.UpdateMetrics(
		config.MetricsEnabled,
		currentNamespaceToMetrics,
		previousNamespacesToMetrics,
		time.Since(start).Seconds(),
	)

	return currentNamespaceToMetrics, nil
}

// processWorkload scans one workload inside its own span and records a failure in logs, the span and metrics.
func processWorkload(
	workload scalable.Workload,
	client kubernetes.Client,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	namespaceScopes map[string]*values.Scope,
	currentNamespaceToMetrics map[string]*metrics.NamespaceMetricsHolder,
	config *runtimeConfiguration,
	downscalerMetrics *metrics.Metrics,
) {
	kind := workloadResourceKind(workload)
	logger := workloadLogger(workload)

	ctx, span := tracing.Tracer().Start(ctx, "downscaler.workload", trace.WithAttributes(
		attribute.String("k8s.namespace.name", workload.GetNamespace()),
		attribute.String("downscaler.workload.kind", kind),
		attribute.String("downscaler.workload.name", workload.GetName()),
	))
	defer span.End()

	logger.Debug("scanning workload")

	workloadNamespaceMetrics, err := getWorkloadNamespaceMetrics(config, workload, currentNamespaceToMetrics)
	if err != nil && !errors.Is(err, ErrMetricsDisabled) {
		logger.Error("failed to get namespace metrics", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to get namespace metrics")

		return
	}

	err = scanWorkload(workload, client, ctx, scopeDefault, scopeCli, scopeEnv, namespaceScopes, workloadNamespaceMetrics, config, logger)
	if err != nil {
		logger.Error("failed to scan workload", "error", err)
		span.RecordError(err)
		span.SetStatus(codes.Error, "failed to scan workload")
		downscalerMetrics.IncrementWorkloadScanErrors(kind)

		return
	}

	logger.Debug("workload scan completed")
}

func handleNamespaceScopeParsingErrors(
	errs []error,
	config *runtimeConfiguration,
	currentNamespaceToMetrics map[string]*metrics.NamespaceMetricsHolder,
) {
	for _, err := range errs {
		slog.Error("failed to get namespace annotations", "error", err)

		var namespaceScopeErr *kubernetes.NamespaceScopeError
		if !errors.As(err, &namespaceScopeErr) {
			continue
		}

		namespaceMetrics, metricsErr := getNamespaceMetrics(config, namespaceScopeErr.Namespace(), currentNamespaceToMetrics)
		if metricsErr != nil {
			if !errors.Is(metricsErr, ErrMetricsDisabled) {
				slog.Error("failed to get namespace metrics", "error", metricsErr, "namespace", namespaceScopeErr.Namespace())
			}

			continue
		}

		namespaceMetrics.MarkParsingNamespaceScopeError()
	}
}

// attemptScaling handles retries for scaling a workload in case of conflicts.
func attemptScaling(
	client kubernetes.Client,
	ctx context.Context,
	decision values.ScalingDecision,
	workload scalable.Workload,
	scopes values.Scopes,
	workloadNamespaceMetrics *metrics.NamespaceMetricsHolder,
	config *runtimeConfiguration,
	logger *slog.Logger,
) error {
	for retry := range config.MaxRetriesOnConflict + 1 {
		err := scaleWorkload(decision.Scaling, workload, scopes, workloadNamespaceMetrics, client, ctx, logger)
		if err != nil {
			if !strings.Contains(err.Error(), registry.OptimisticLockErrorMsg) {
				recordScalingError(err, workloadNamespaceMetrics)
				return err
			}

			logger.Warn("workload modified, retrying", "attempt", retry+1)

			err = client.RegetWorkload(workload, ctx)
			if err != nil {
				return fmt.Errorf("failed to fetch updated workload: %w", err)
			}

			continue
		}

		logger.Debug("successfully processed workload state")

		return nil
	}

	workloadNamespaceMetrics.IncrementConflictErrorsCount()
	logger.Error("failed to scale workload", "attempts", config.MaxRetriesOnConflict+1)

	return newMaxRetriesExceeded(config.MaxRetriesOnConflict)
}

func recordScalingError(err error, workloadNamespaceMetrics *metrics.NamespaceMetricsHolder) {
	var scalingInvalidErr *ScalingInvalidError
	if errors.As(err, &scalingInvalidErr) {
		workloadNamespaceMetrics.IncrementInvalidScalingValueErrorsCount()
		return
	}

	workloadNamespaceMetrics.IncrementGenericErrorsCount()
}

// scanWorkload runs a scan on the workload, determining the scaling and scaling the workload.
func scanWorkload(
	workload scalable.Workload,
	client kubernetes.Client,
	ctx context.Context,
	scopeDefault, scopeCli, scopeEnv *values.Scope,
	namespaceScopes map[string]*values.Scope,
	workloadNamespaceMetrics *metrics.NamespaceMetricsHolder,
	config *runtimeConfiguration,
	logger *slog.Logger,
) error {
	eventLogger := kubernetes.NewEventLoggerForWorkload(client, workload)

	var err error

	logger.Debug(
		"parsing workload scope from annotations",
		"annotations", workload.GetAnnotations(),
	)

	scopeWorkload := values.NewScope()
	if err = scopeWorkload.GetScopeFromAnnotations(workload.GetAnnotations(), eventLogger, logger, ctx); err != nil {
		workloadNamespaceMetrics.IncrementParsingWorkloadScopeErrorsCount()
		return fmt.Errorf("failed to parse workload scope from annotations: %w", err)
	}

	scopeNamespace, exists := namespaceScopes[workload.GetNamespace()]
	if !exists {
		return newNamespaceScopeRetrieveError(workload.GetNamespace())
	}

	scopes := values.Scopes{scopeWorkload, scopeNamespace, scopeCli, scopeEnv, scopeDefault}

	logger.Debug("finished parsing all scopes", "scopes", scopes)

	gracePeriodEvaluation, err := scopes.IsInGracePeriod(
		config.TimeAnnotation,
		workload.GetAnnotations(),
		workload.GetCreationTimestamp().Time,
		eventLogger,
		logger,
		ctx,
	)
	if err != nil {
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()
		return fmt.Errorf("failed to get if workload is on grace period: %w", err)
	}

	if gracePeriodEvaluation.Matched {
		logger = logger.With(
			"scalingAction", "scalingGracePeriod",
			"decisionScope", gracePeriodEvaluation.Scope.String(),
		)
		logger.Debug("workload is on grace period, skipping")
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("downscaler.scaling", "scalingGracePeriod"))
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()

		return nil
	}

	exclusionEvaluation := scopes.GetExcludedWithScope(scopes)
	upscaleOnExclusion, upscaleScope := scopes.GetUpscaleExcluded()

	if exclusionEvaluation.Matched && !upscaleOnExclusion {
		logger = logger.With(
			"scalingAction", "scalingExcluded",
			"decisionScope", exclusionEvaluation.Scope.String(),
		)
		logger.Debug("workload is excluded, skipping")
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("downscaler.scaling", "scalingExcluded"))
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()

		return nil
	}

	decision := getCurrentScaling(exclusionEvaluation.Matched, upscaleOnExclusion, upscaleScope, &scopes, logger)
	logger = withScalingDecision(logger, decision)
	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("downscaler.scaling", decision.Scaling.String()),
		attribute.String("downscaler.decision_scope", decision.Scope.String()),
	)

	err = attemptScaling(client, ctx, decision, workload, scopes, workloadNamespaceMetrics, config, logger)
	if err != nil {
		return err
	}

	if scopes.GetScaleChildren() {
		childrenWorkloads, err := client.GetChildrenWorkloads(workload, ctx)
		if err != nil {
			return fmt.Errorf("failed to get children workloads: %w", err)
		}

		logger.Debug("scaling children workloads", "childrenCount", len(childrenWorkloads))
		scaleWorkloads(decision, childrenWorkloads, scopes, workloadNamespaceMetrics, client, ctx, config)
	}

	return nil
}

func workloadResourceKind(workload scalable.Workload) string {
	kind := "workload"

	func() {
		defer func() { _ = recover() }()

		if resourceKind := workload.GroupVersionKind().Kind; resourceKind != "" {
			kind = resourceKind
		}
	}()

	return kind
}

func workloadLogger(workload scalable.Workload) *slog.Logger {
	return slog.Default().With(
		"workload", workload.GetName(),
		"namespace", workload.GetNamespace(),
		"kind", workloadResourceKind(workload),
	)
}

func withScalingDecision(logger *slog.Logger, decision values.ScalingDecision) *slog.Logger {
	valueKey := "decisionValue"
	if decision.Reason == values.DecisionReasonUpscaleOnExclusion {
		valueKey = "upscaleOnExclusion"
	}

	return logger.With(
		"scalingAction", decision.Scaling.String(),
		"decisionScope", decision.Scope.String(),
		valueKey, decision.Value.LogValue(),
	)
}

func getCurrentScaling(
	excluded, upscaleOnExclusion bool,
	upscaleScope values.ScopeID,
	scopes *values.Scopes,
	logger *slog.Logger,
) values.ScalingDecision {
	if upscaleOnExclusion && excluded {
		logger.Debug("upscaling excluded workload")

		return values.ScalingDecision{
			Scaling: values.ScalingUp,
			Scope:   upscaleScope,
			Value:   values.NewBooleanScalingValue(true),
			Reason:  values.DecisionReasonUpscaleOnExclusion,
		}
	}

	return scopes.GetCurrentScaling()
}

// scaleWorkloads scales the given workloads to the specified scaling asynchronously.
func scaleWorkloads(
	decision values.ScalingDecision,
	workloads []scalable.Workload,
	scopes values.Scopes,
	workloadNamespaceMetrics *metrics.NamespaceMetricsHolder,
	client kubernetes.Client,
	ctx context.Context,
	config *runtimeConfiguration,
) {
	for _, workload := range workloads {
		go func(workload scalable.Workload) {
			logger := withScalingDecision(workloadLogger(workload), decision)

			err := attemptScaling(client, ctx, decision, workload, scopes, workloadNamespaceMetrics, config, logger)
			if err != nil {
				logger.Error("scaling operation failed", "error", err)
			}
		}(workload)
	}
}

// scaleWorkload scales the given workload according to the given wanted scaling state.
func scaleWorkload(
	scaling values.Scaling,
	workload scalable.Workload,
	scopes values.Scopes,
	workloadNamespaceMetrics *metrics.NamespaceMetricsHolder,
	client kubernetes.Client,
	ctx context.Context,
	logger *slog.Logger,
) error {
	if scaling == values.ScalingNone {
		logger.Debug("scaling is not set by any scope, skipping")
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()

		return nil
	}

	if scaling == values.ScalingIgnore {
		logger.Debug("scaling is ignored, skipping")
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()

		return nil
	}

	if scaling == values.ScalingIncomplete {
		logger.Warn("scaling times cannot be determined, skipping")
		workloadNamespaceMetrics.IncrementExcludedWorkloadsCount()

		return nil
	}

	if scaling == values.ScalingMultiple {
		return newScalingInvalidError(
			`scaling values matched to multiple states.
this is the result of a faulty configuration where on a scope there is multiple values with the same priority
setting different scaling states at the same time (e.g. downtime-period and uptime-period or force-downtime and force-uptime)`,
		)
	}

	if scaling == values.ScalingDown {
		logger.Debug("downscaling workload")

		downscaleReplicas, err := scopes.GetDownscaleReplicas()
		if err != nil {
			return fmt.Errorf("failed to get downscale replicas: %w", err)
		}

		savedResources, err := client.DownscaleWorkload(downscaleReplicas, workload, ctx, logger)
		if err != nil {
			return fmt.Errorf("failed to downscale workload: %w", err)
		}

		workloadNamespaceMetrics.IncrementDownscaledWorkloadsCount()
		workloadNamespaceMetrics.IncrementSavedResources(savedResources)
	}

	if scaling == values.ScalingUp {
		logger.Debug("upscaling workload")

		err := client.UpscaleWorkload(workload, ctx, logger)
		if err != nil {
			return fmt.Errorf("failed to upscale workload: %w", err)
		}

		workloadNamespaceMetrics.IncrementUpscaledWorkloadsCount()
	}

	return nil
}

func initMetrics(config *runtimeConfiguration) *metrics.Metrics {
	if !config.MetricsEnabled {
		return nil
	}

	m := metrics.NewMetrics(config.DryRun)
	m.RegisterAll()
	slog.Info("metrics initialized")

	return m
}

// getWorkloadNamespaceMetrics retrieves the metrics holder for the workload's namespace.
func getWorkloadNamespaceMetrics(
	config *runtimeConfiguration,
	workload scalable.Workload,
	currentNamespaceToMetrics map[string]*metrics.NamespaceMetricsHolder,
) (*metrics.NamespaceMetricsHolder, error) {
	if !config.MetricsEnabled {
		return nil, ErrMetricsDisabled
	}

	workloadNamespaceMetrics, ok := currentNamespaceToMetrics[workload.GetNamespace()]
	if !ok {
		return nil, NewMetricHolderNotFoundError(workload.GetNamespace())
	}

	return workloadNamespaceMetrics, nil
}

func getNamespaceMetrics(
	config *runtimeConfiguration,
	namespace string,
	currentNamespaceToMetrics map[string]*metrics.NamespaceMetricsHolder,
) (*metrics.NamespaceMetricsHolder, error) {
	if !config.MetricsEnabled {
		return nil, ErrMetricsDisabled
	}

	namespaceMetrics, ok := currentNamespaceToMetrics[namespace]
	if !ok {
		return nil, NewMetricHolderNotFoundError(namespace)
	}

	return namespaceMetrics, nil
}

// newNamespaceToMetrics creates a new map for namespace to metrics holder if metrics are enabled.
func newNamespaceToMetrics(config *runtimeConfiguration) map[string]*metrics.NamespaceMetricsHolder {
	if config.MetricsEnabled {
		return make(map[string]*metrics.NamespaceMetricsHolder)
	}

	return nil
}
