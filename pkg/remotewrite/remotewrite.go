package remotewrite

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/grafana/xk6-output-prometheus-remote/pkg/remote"

	"go.k6.io/k6/metrics"
	"go.k6.io/k6/output"

	"github.com/sirupsen/logrus"
	prompb "go.buf.build/grpc/go/prometheus/prometheus"
)

var _ output.Output = new(Output)

type Output struct {
	output.SampleBuffer

	config             Config
	logger             logrus.FieldLogger
	periodicFlusher    *output.PeriodicFlusher
	tsdb               map[metrics.TimeSeries]*seriesWithMeasure
	trendStatsResolver map[string]func(*metrics.TrendSink) float64

	// TODO: copy the prometheus/remote.WriteClient interface and depend on it
	client *remote.WriteClient
}

func New(params output.Params) (*Output, error) {
	logger := params.Logger.WithFields(logrus.Fields{"output": "Prometheus remote write"})

	config, err := GetConsolidatedConfig(params.JSONConfig, params.Environment, params.ConfigArgument)
	if err != nil {
		return nil, err
	}

	clientConfig, err := config.RemoteConfig()
	if err != nil {
		return nil, err
	}

	wc, err := remote.NewWriteClient(config.URL.String, clientConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize the Prometheus remote write client: %w", err)
	}

	o := &Output{
		client: wc,
		config: config,
		logger: logger,
		tsdb:   make(map[metrics.TimeSeries]*seriesWithMeasure),
	}

	if len(config.TrendStats) > 0 {
		if err := o.setTrendStatsResolver(config.TrendStats); err != nil {
			return nil, err
		}
	}
	return o, nil
}

func (o *Output) Description() string {
	return fmt.Sprintf("Prometheus remote write (%s)", o.config.URL.String)
}

func (o *Output) Start() error {
	d := o.config.PushInterval.TimeDuration()
	periodicFlusher, err := output.NewPeriodicFlusher(d, o.flush)
	if err != nil {
		return err
	}
	o.periodicFlusher = periodicFlusher
	o.logger.WithField("flushtime", d).Debug("Output initialized")
	return nil
}

func (o *Output) Stop() error {
	o.logger.Debug("Stopping the output")
	o.periodicFlusher.Stop()
	o.logger.Debug("Output stopped")
	return nil
}

// setTrendStatsResolver sets the resolver for the Trend stats.
//
// TODO: refactor, the code can be improved
func (o *Output) setTrendStatsResolver(trendStats []string) error {
	var trendStatsCopy []string
	hasSum := false
	// copy excluding sum
	for _, stat := range trendStats {
		if stat == "sum" {
			hasSum = true
			continue
		}
		trendStatsCopy = append(trendStatsCopy, stat)
	}
	resolvers, err := metrics.GetResolversForTrendColumns(trendStatsCopy)
	if err != nil {
		return err
	}
	// sum is not supported from GetResolversForTrendColumns
	// so if it has been requested
	// it adds it specifically
	if hasSum {
		resolvers["sum"] = func(t *metrics.TrendSink) float64 {
			return t.Sum
		}
	}
	o.trendStatsResolver = make(TrendStatsResolver, len(resolvers))
	for stat, fn := range resolvers {
		statKey := stat

		// the config passes percentiles with p(x) form, for example p(95),
		// but the mapping generates series name in the form p95.
		//
		// TODO: maybe decoupling mapping from the stat resolver keys?
		if strings.HasPrefix(statKey, "p(") {
			statKey = stat[2 : len(statKey)-1]             // trim the parenthesis
			statKey = strings.ReplaceAll(statKey, ".", "") // remove dots, p(0.95) => p095
			statKey = "p" + statKey
		}
		o.trendStatsResolver[statKey] = fn
	}
	return nil
}

func (o *Output) flush() {
	var (
		start = time.Now()
		nts   int
	)

	defer func() {
		d := time.Since(start)
		okmsg := "Successful flushed time series to remote write endpoint"
		if d > time.Duration(o.config.PushInterval.Duration) {
			// There is no intermediary storage so warn if writing to remote write endpoint becomes too slow
			o.logger.WithField("nts", nts).
				Warnf("%s but it took %s while flush period is %s. Some samples may be dropped.",
					okmsg, d.String(), o.config.PushInterval.String())
		} else {
			o.logger.WithField("nts", nts).WithField("took", d).Debug(okmsg)
		}
	}()

	samplesContainers := o.GetBufferedSamples()
	if len(samplesContainers) < 1 {
		o.logger.Debug("no buffered samples, skip the flushing operation")
		return
	}

	// Remote write endpoint accepts TimeSeries structure defined in gRPC. It must:
	// a) contain Labels array
	// b) have a __name__ label: without it, metric might be unquerable or even rejected
	// as a metric without a name. This behaviour depends on underlying storage used.
	// c) not have duplicate timestamps within 1 timeseries, see https://github.com/prometheus/prometheus/issues/9210
	// Prometheus write handler processes only some fields as of now, so here we'll add only them.

	promTimeSeries := o.convertToPbSeries(samplesContainers)
	nts = len(promTimeSeries)
	o.logger.WithField("nts", nts).Debug("Converted samples to Prometheus TimeSeries")

	if err := o.client.Store(context.Background(), promTimeSeries); err != nil {
		o.logger.WithError(err).Error("Failed to send the time series data to the endpoint")
		return
	}
}

func (o *Output) convertToPbSeries(samplesContainers []metrics.SampleContainer) []*prompb.TimeSeries {
	// The seen map is required because the samples containers
	// could have several samples for the same time series
	//  in this way, we can aggregate and flush them in a unique value
	//  without overloading the remote write endpoint.
	//
	// It is also essential because the core generates timestamps
	// with a higher precision (ns) than Prometheus (ms),
	// so we need to aggregate all the samples in the same time bucket.
	// More context can be found in the issue
	// https://github.com/grafana/xk6-output-prometheus-remote/issues/11
	seen := make(map[metrics.TimeSeries]struct{})

	for _, samplesContainer := range samplesContainers {
		samples := samplesContainer.GetSamples()

		for _, sample := range samples {
			truncTime := sample.Time.Truncate(time.Millisecond)
			swm, ok := o.tsdb[sample.TimeSeries]
			if !ok {
				// TODO: encapsulate the trend arguments into a Trend Mapping factory
				swm = newSeriesWithMeasure(sample.TimeSeries, o.config.TrendAsNativeHistogram.Bool, o.trendStatsResolver)
				swm.Latest = truncTime
				o.tsdb[sample.TimeSeries] = swm
				seen[sample.TimeSeries] = struct{}{}
			} else {
				// save as a seen item only when the samples have a time greater than
				// the previous saved, otherwise some implementations
				// could see it as a duplicate and generate warnings (e.g. Mimir)
				if truncTime.After(swm.Latest) {
					swm.Latest = truncTime
					seen[sample.TimeSeries] = struct{}{}
				}

				// If current == previous:
				// the current received time before being truncated had a higher precision.
				// It's fine to aggregate them but we avoid to add to the seen map because:
				// - in the case it is a new flush operation then we avoid delivering
				//   for not generating duplicates
				// - in the case it is in the same operation but across sample containers
				//   then the time series should be already on the seen map and we can skip
				//   to re-add it.

				// If current < previous:
				// - in the case current is a new flush operation, it shouldn't happen,
				//   for this reason, we can avoid creating a dedicated logic.
				//   TODO: We should evaluate if it would be better to have a defensive condition
				//   for handling it, logging a warning or returning an error
				//   and avoid aggregating the value.
				// - in the case case current is in the same operation but across sample containers
				//   it's fine to aggregate
				//   but same as for the equal condition it can rely on the previous seen value.
			}
			swm.Measure.Add(sample)
		}
	}

	pbseries := make([]*prompb.TimeSeries, 0, len(seen))
	for s := range seen {
		pbseries = append(pbseries, o.tsdb[s].MapPrompb()...)
	}
	return pbseries
}

type seriesWithMeasure struct {
	metrics.TimeSeries
	Measure metrics.Sink

	// Latest tracks the latest time
	// when the measure has been updated
	//
	// TODO: the logic for this value should stay directly
	// in a method in struct
	Latest time.Time

	// TODO: maybe add some caching for the mapping?
}

// TODO: unit test this
func (swm seriesWithMeasure) MapPrompb() []*prompb.TimeSeries {
	var newts []*prompb.TimeSeries

	mapMonoSeries := func(s metrics.TimeSeries, t time.Time) prompb.TimeSeries {
		return prompb.TimeSeries{
			// TODO: should we add the suffix for
			// Counter, Rate and Gauge?
			Labels: MapSeries(s, ""),
			Samples: []*prompb.Sample{
				{Timestamp: t.UnixMilli()},
			},
		}
	}

	switch swm.Metric.Type {
	case metrics.Counter:
		ts := mapMonoSeries(swm.TimeSeries, swm.Latest)
		ts.Samples[0].Value = swm.Measure.(*metrics.CounterSink).Value
		newts = []*prompb.TimeSeries{&ts}

	case metrics.Gauge:
		ts := mapMonoSeries(swm.TimeSeries, swm.Latest)
		ts.Samples[0].Value = swm.Measure.(*metrics.GaugeSink).Value
		newts = []*prompb.TimeSeries{&ts}

	case metrics.Rate:
		ts := mapMonoSeries(swm.TimeSeries, swm.Latest)
		// pass zero duration here because time is useless for formatting rate
		rateVals := swm.Measure.(*metrics.RateSink).Format(time.Duration(0))
		ts.Samples[0].Value = rateVals["rate"]
		newts = []*prompb.TimeSeries{&ts}

	case metrics.Trend:
		// TODO:
		//	- Add a PrompbMapSinker interface
		//    and implements it on all the sinks "extending" them.
		//  - Call directly MapPrompb on Measure without any type assertion.
		trend, ok := swm.Measure.(prompbMapper)
		if !ok {
			panic("Measure for Trend types must implement MapPromPb")
		}
		newts = trend.MapPrompb(swm.TimeSeries, swm.Latest)
	default:
		panic(fmt.Sprintf("Something is really off, as I cannot recognize the type of metric %s: `%s`", swm.Metric.Name, swm.Metric.Type))
	}
	return newts
}

type prompbMapper interface {
	MapPrompb(series metrics.TimeSeries, t time.Time) []*prompb.TimeSeries
}

func newSeriesWithMeasure(series metrics.TimeSeries, trendAsNativeHistogram bool, tsr TrendStatsResolver) *seriesWithMeasure {
	var sink metrics.Sink
	switch series.Metric.Type {
	case metrics.Counter:
		sink = &metrics.CounterSink{}
	case metrics.Gauge:
		sink = &metrics.GaugeSink{}
	case metrics.Trend:
		// TODO: refactor encapsulating in a factory method
		if trendAsNativeHistogram {
			sink = newNativeHistogramSink(series.Metric)
		} else {
			var err error
			sink, err = newExtendedTrendSink(tsr)
			if err != nil {
				// the resolver must be already validated
				panic(err)
			}
		}
	case metrics.Rate:
		sink = &metrics.RateSink{}
	default:
		panic(fmt.Sprintf("metric type %q unsupported", series.Metric.Type.String()))
	}
	return &seriesWithMeasure{
		TimeSeries: series,
		Measure:    sink,
	}
}
