package fetcher

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
)

// Metrics holds the OTLP metric instruments for the docker root fetcher.
type Metrics struct {
	// Ctx is a long-lived context used for recording metrics. The OTel SDK
	// silently drops measurements when ctx.Err() != nil, so this must never
	// be tied to a per-request context that could time out or be cancelled.
	Ctx context.Context

	AcquireTotal      metric.Int64Counter
	AcquireDuration   metric.Float64Histogram
	RootsGauge        metric.Int64UpDownCounter
	ImagePullCount    metric.Int64Counter
	ImagePrepDuration metric.Float64Histogram
}

// InitOTLPMetrics initializes OTLP metrics for the given app name.
// If endpoints is empty, returns no-op metrics.
func InitOTLPMetrics(ctx context.Context, appName string, endpoints []string) (*Metrics, func(context.Context) error, error) {
	if len(endpoints) == 0 {
		log.Printf("No OTLP collector endpoints configured — metrics disabled")
		m, _, err := newMetrics(appName)
		return m, nil, err
	}

	podName := os.Getenv("POD_NAME")
	if podName == "" {
		podName, _ = os.Hostname()
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.Key("pod_name").String(podName),
			attribute.Key("app").String(appName),
		))
	if err != nil {
		return nil, nil, fmt.Errorf("create otel resource: %w", err)
	}

	var opts []sdkmetric.Option
	for _, endpoint := range endpoints {
		exporter, err := otlpmetricgrpc.New(
			ctx,
			otlpmetricgrpc.WithEndpoint(endpoint),
			otlpmetricgrpc.WithCompressor("gzip"),
			otlpmetricgrpc.WithInsecure(),
			otlpmetricgrpc.WithTimeout(30*time.Second),
		)
		if err != nil {
			return nil, nil, fmt.Errorf("create otlp exporter for %s: %w", endpoint, err)
		}
		opts = append(opts, sdkmetric.WithReader(
			sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(60*time.Second)),
		))
		log.Printf("metrics: exporter configured for %s", endpoint)
	}
	opts = append(opts, sdkmetric.WithResource(res))

	provider := sdkmetric.NewMeterProvider(opts...)
	otel.SetMeterProvider(provider)

	m, _, err := newMetrics(appName)
	if err != nil {
		return nil, nil, err
	}
	return m, provider.Shutdown, nil
}

// exponentialBoundaries returns count histogram bucket boundaries growing
// geometrically from start by factor: start, start*factor, start*factor^2, ...
func exponentialBoundaries(start, factor float64, count int) []float64 {
	bounds := make([]float64, count)
	b := start
	for i := range bounds {
		bounds[i] = b
		b *= factor
	}
	return bounds
}

func newMetrics(appName string) (*Metrics, func(context.Context) error, error) {
	meter := otel.Meter(appName)
	prefix := appName + "_"

	acquireTotal, err := meter.Int64Counter(prefix + "acquire_total")
	if err != nil {
		return nil, nil, err
	}
	// The OTel SDK default histogram boundaries (0, 5, 10, ... 10000) are tuned
	// for milliseconds, but durations here are recorded in seconds, so we need to define our own.
	acquireDuration, err := meter.Float64Histogram(
		prefix+"acquire_duration_seconds",
		metric.WithExplicitBucketBoundaries(exponentialBoundaries(0.001, 2, 21)...),
	)
	if err != nil {
		return nil, nil, err
	}
	rootsGauge, err := meter.Int64UpDownCounter(prefix + "materialized_roots")
	if err != nil {
		return nil, nil, err
	}
	imagePullCount, err := meter.Int64Counter(prefix + "image_pull_count")
	if err != nil {
		return nil, nil, err
	}
	imagePrepDuration, err := meter.Float64Histogram(
		prefix+"image_prep_duration_seconds",
		metric.WithExplicitBucketBoundaries(exponentialBoundaries(1, 2, 11)...),
	)
	if err != nil {
		return nil, nil, err
	}

	return &Metrics{
		Ctx:               context.Background(),
		AcquireTotal:      acquireTotal,
		AcquireDuration:   acquireDuration,
		RootsGauge:        rootsGauge,
		ImagePullCount:    imagePullCount,
		ImagePrepDuration: imagePrepDuration,
	}, nil, nil
}
