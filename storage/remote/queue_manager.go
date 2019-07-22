// Copyright 2013 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package remote

import (
	"context"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/gogo/protobuf/proto"
	"github.com/golang/snappy"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/prompb"
	"github.com/prometheus/tsdb"
	tsdbLabels "github.com/prometheus/tsdb/labels"
)

// String constants for instrumentation.
const (
	namespace = "prometheus"
	subsystem = "remote_storage"
	queue     = "queue"

	// We track samples in/out and how long pushes take using an Exponentially
	// Weighted Moving Average.
	// 我们通过EWMA追踪sample的进出以及推送花费的时间
	ewmaWeight          = 0.2
	shardUpdateDuration = 10 * time.Second

	// Allow 30% too many shards before scaling down.
	shardToleranceFraction = 0.3
)

var (
	succeededSamplesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "succeeded_samples_total",
			Help:      "Total number of samples successfully sent to remote storage.",
		},
		[]string{queue},
	)
	failedSamplesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "failed_samples_total",
			Help:      "Total number of samples which failed on send to remote storage, non-recoverable errors.",
		},
		[]string{queue},
	)
	retriedSamplesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "retried_samples_total",
			Help:      "Total number of samples which failed on send to remote storage but were retried because the send error was recoverable.",
		},
		[]string{queue},
	)
	droppedSamplesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "dropped_samples_total",
			Help:      "Total number of samples which were dropped after being read from the WAL before being sent via remote write.",
		},
		[]string{queue},
	)
	enqueueRetriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "enqueue_retries_total",
			Help:      "Total number of times enqueue has failed because a shards queue was full.",
		},
		[]string{queue},
	)
	sentBatchDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "sent_batch_duration_seconds",
			Help:      "Duration of sample batch send calls to the remote storage.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{queue},
	)
	queueHighestSentTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "queue_highest_sent_timestamp_seconds",
			Help:      "Timestamp from a WAL sample, the highest timestamp successfully sent by this queue, in seconds since epoch.",
		},
		[]string{queue},
	)
	queuePendingSamples = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "pending_samples",
			Help:      "The number of samples pending in the queues shards to be sent to the remote storage.",
		},
		[]string{queue},
	)
	shardCapacity = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "shard_capacity",
			Help:      "The capacity of each shard of the queue used for parallel sending to the remote storage.",
		},
		[]string{queue},
	)
	numShards = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "shards",
			Help:      "The number of shards used for parallel sending to the remote storage.",
		},
		[]string{queue},
	)
)

// StorageClient defines an interface for sending a batch of samples to an
// external timeseries database.
type StorageClient interface {
	// Store stores the given samples in the remote storage.
	Store(context.Context, []byte) error
	// Name identifies the remote storage implementation.
	Name() string
}

// QueueManager manages a queue of samples to be sent to the Storage
// indicated by the provided StorageClient. Implements writeTo interface
// used by WAL Watcher.
type QueueManager struct {
	logger         log.Logger
	flushDeadline  time.Duration
	cfg            config.QueueConfig
	externalLabels labels.Labels
	relabelConfigs []*relabel.Config
	client         StorageClient
	watcher        *WALWatcher

	// seriesLabels维持series ref到labels集合的映射
	seriesLabels         map[uint64][]prompb.Label
	seriesSegmentIndexes map[uint64]int
	droppedSeries        map[uint64]struct{}

	shards      *shards
	numShards   int
	reshardChan chan int
	quit        chan struct{}
	wg          sync.WaitGroup

	samplesIn, samplesDropped, samplesOut, samplesOutDuration *ewmaRate
	integralAccumulator                                       float64

	highestSentTimestampMetric *maxGauge
	pendingSamplesMetric       prometheus.Gauge
	enqueueRetriesMetric       prometheus.Counter
	droppedSamplesTotal        prometheus.Counter
	numShardsMetric            prometheus.Gauge
	failedSamplesTotal         prometheus.Counter
	sentBatchDuration          prometheus.Observer
	succeededSamplesTotal      prometheus.Counter
	retriedSamplesTotal        prometheus.Counter
	shardCapacity              prometheus.Gauge
}

// NewQueueManager builds a new QueueManager.
func NewQueueManager(logger log.Logger, walDir string, samplesIn *ewmaRate, cfg config.QueueConfig, externalLabels labels.Labels, relabelConfigs []*relabel.Config, client StorageClient, flushDeadline time.Duration) *QueueManager {
	if logger == nil {
		logger = log.NewNopLogger()
	}

	name := client.Name()
	logger = log.With(logger, "queue", name)
	t := &QueueManager{
		logger:         logger,
		flushDeadline:  flushDeadline,
		cfg:            cfg,
		externalLabels: externalLabels,
		relabelConfigs: relabelConfigs,
		client:         client,

		seriesLabels:         make(map[uint64][]prompb.Label),
		seriesSegmentIndexes: make(map[uint64]int),
		droppedSeries:        make(map[uint64]struct{}),

		// 默认启动的shard为MinShards
		numShards:   cfg.MinShards,
		reshardChan: make(chan int),
		quit:        make(chan struct{}),

		samplesIn:          samplesIn,
		// 每十秒钟更新一次，ewmaWeight为0.2
		samplesDropped:     newEWMARate(ewmaWeight, shardUpdateDuration),
		samplesOut:         newEWMARate(ewmaWeight, shardUpdateDuration),
		samplesOutDuration: newEWMARate(ewmaWeight, shardUpdateDuration),
	}

	// 创建wal watcher
	t.watcher = NewWALWatcher(logger, name, t, walDir)
	// 创建shard
	t.shards = t.newShards()

	return t
}

// Append queues a sample to be sent to the remote storage. Blocks until all samples are
// enqueued on their shards or a shutdown signal is received.
// Append将一个sample入队，等待发送到远程的存储，阻塞直到所有的sample都进入到它们的shard或者收到了一个shutdonw signal
func (t *QueueManager) Append(s []tsdb.RefSample) bool {
outer:
	for _, sample := range s {
		lbls, ok := t.seriesLabels[sample.Ref]
		if !ok {
			t.droppedSamplesTotal.Inc()
			// 直接被drop的sample
			t.samplesDropped.incr(1)
			if _, ok := t.droppedSeries[sample.Ref]; !ok {
				level.Info(t.logger).Log("msg", "dropped sample for series that was not explicitly dropped via relabelling", "ref", sample.Ref)
			}
			continue
		}
		// This will only loop if the queues are being resharded.
		// 只有队列在reshard的时候才会进行循环
		backoff := t.cfg.MinBackoff
		for {
			select {
			case <-t.quit:
				return false
			default:
			}

			ts := prompb.TimeSeries{
				Labels: lbls,
				Samples: []prompb.Sample{
					prompb.Sample{
						Value:     float64(sample.V),
						Timestamp: sample.T,
					},
				},
			}
			if t.shards.enqueue(sample.Ref, ts) {
				continue outer
			}

			t.enqueueRetriesMetric.Inc()
			time.Sleep(time.Duration(backoff))
			backoff = backoff * 2
			if backoff > t.cfg.MaxBackoff {
				backoff = t.cfg.MaxBackoff
			}
		}
	}
	return true
}

// Start the queue manager sending samples to the remote storage.
// Does not block.
// Start启动queue manager向远程存储发送samples
func (t *QueueManager) Start() {
	// Setup the QueueManagers metrics. We do this here rather than in the
	// constructor because of the ordering of creating Queue Managers's, stopping them,
	// and then starting new ones in storage/remote/storage.go ApplyConfig.
	name := t.client.Name()
	t.highestSentTimestampMetric = &maxGauge{
		Gauge: queueHighestSentTimestamp.WithLabelValues(name),
	}
	t.pendingSamplesMetric = queuePendingSamples.WithLabelValues(name)
	t.enqueueRetriesMetric = enqueueRetriesTotal.WithLabelValues(name)
	t.droppedSamplesTotal = droppedSamplesTotal.WithLabelValues(name)
	t.numShardsMetric = numShards.WithLabelValues(name)
	t.failedSamplesTotal = failedSamplesTotal.WithLabelValues(name)
	t.sentBatchDuration = sentBatchDuration.WithLabelValues(name)
	t.succeededSamplesTotal = succeededSamplesTotal.WithLabelValues(name)
	t.retriedSamplesTotal = retriedSamplesTotal.WithLabelValues(name)
	t.shardCapacity = shardCapacity.WithLabelValues(name)

	// Initialise some metrics.
	t.shardCapacity.Set(float64(t.cfg.Capacity))
	t.pendingSamplesMetric.Set(0)

	// 启动shards和wal watcher
	t.shards.start(t.numShards)
	t.watcher.Start()

	t.wg.Add(2)
	go t.updateShardsLoop()
	go t.reshardLoop()
}

// Stop stops sending samples to the remote storage and waits for pending
// sends to complete.
func (t *QueueManager) Stop() {
	level.Info(t.logger).Log("msg", "Stopping remote storage...")
	defer level.Info(t.logger).Log("msg", "Remote storage stopped.")

	close(t.quit)
	t.wg.Wait()
	// Wait for all QueueManager routines to end before stopping shards and WAL watcher. This
	// is to ensure we don't end up executing a reshard and shards.stop() at the same time, which
	// causes a closed channel panic.
	t.shards.stop()
	t.watcher.Stop()

	// On shutdown, release the strings in the labels from the intern pool.
	for _, labels := range t.seriesLabels {
		release(labels)
	}
	// Delete metrics so we don't have alerts for queues that are gone.
	name := t.client.Name()
	queueHighestSentTimestamp.DeleteLabelValues(name)
	queuePendingSamples.DeleteLabelValues(name)
	enqueueRetriesTotal.DeleteLabelValues(name)
	droppedSamplesTotal.DeleteLabelValues(name)
	numShards.DeleteLabelValues(name)
	failedSamplesTotal.DeleteLabelValues(name)
	sentBatchDuration.DeleteLabelValues(name)
	succeededSamplesTotal.DeleteLabelValues(name)
	retriedSamplesTotal.DeleteLabelValues(name)
	shardCapacity.DeleteLabelValues(name)
}

// StoreSeries keeps track of which series we know about for lookups when sending samples to remote.
// StoreSeries追踪我们知道的series，可以在发送samples到remote的时候进行查询
func (t *QueueManager) StoreSeries(series []tsdb.RefSeries, index int) {
	for _, s := range series {
		ls := processExternalLabels(s.Labels, t.externalLabels)
		rl := relabel.Process(ls, t.relabelConfigs...)
		if len(rl) == 0 {
			t.droppedSeries[s.Ref] = struct{}{}
			continue
		}
		t.seriesSegmentIndexes[s.Ref] = index
		labels := labelsToLabelsProto(rl)

		// We should not ever be replacing a series labels in the map, but just
		// in case we do we need to ensure we do not leak the replaced interned
		// strings.
		if orig, ok := t.seriesLabels[s.Ref]; ok {
			release(orig)
		}
		// 建立s.Ref和labels的映射关系
		t.seriesLabels[s.Ref] = labels
	}
}

// SeriesReset is used when reading a checkpoint. WAL Watcher should have
// stored series records with the checkpoints index number, so we can now
// delete any ref ID's lower than that # from the two maps.
func (t *QueueManager) SeriesReset(index int) {
	// Check for series that are in segments older than the checkpoint
	// that were not also present in the checkpoint.
	for k, v := range t.seriesSegmentIndexes {
		if v < index {
			delete(t.seriesSegmentIndexes, k)
			release(t.seriesLabels[k])
			delete(t.seriesLabels, k)
		}
	}
}

func release(ls []prompb.Label) {
	for _, l := range ls {
		interner.release(l.Name)
		interner.release(l.Value)
	}
}

// processExternalLabels merges externalLabels into ls. If ls contains
// a label in externalLabels, the value in ls wins.
func processExternalLabels(ls tsdbLabels.Labels, externalLabels labels.Labels) labels.Labels {
	i, j, result := 0, 0, make(labels.Labels, 0, len(ls)+len(externalLabels))
	for i < len(ls) && j < len(externalLabels) {
		if ls[i].Name < externalLabels[j].Name {
			result = append(result, labels.Label{
				Name:  ls[i].Name,
				Value: ls[i].Value,
			})
			i++
		} else if ls[i].Name > externalLabels[j].Name {
			result = append(result, externalLabels[j])
			j++
		} else {
			result = append(result, labels.Label{
				Name:  ls[i].Name,
				Value: ls[i].Value,
			})
			i++
			j++
		}
	}
	for ; i < len(ls); i++ {
		result = append(result, labels.Label{
			Name:  ls[i].Name,
			Value: ls[i].Value,
		})
	}
	result = append(result, externalLabels[j:]...)
	return result
}

func (t *QueueManager) updateShardsLoop() {
	defer t.wg.Done()

	ticker := time.NewTicker(shardUpdateDuration)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			t.calculateDesiredShards()
		case <-t.quit:
			return
		}
	}
}

func (t *QueueManager) calculateDesiredShards() {
	t.samplesOut.tick()
	t.samplesDropped.tick()
	t.samplesOutDuration.tick()

	// We use the number of incoming samples as a prediction of how much work we
	// will need to do next iteration.  We add to this any pending samples
	// (received - send) so we can catch up with any backlog. We use the average
	// outgoing batch latency to work out how many shards we need.
	// 我们使用接收到的samples数目作为对于下一轮我们需要做多少工作的预测
	// 我们使用发出的batch的延迟来决定我们需要多少shards
	var (
		// 上一个interval中sample进和出的速度
		samplesIn          = t.samplesIn.rate()
		samplesOut         = t.samplesOut.rate()
		samplesKeptRatio   = samplesOut / (t.samplesDropped.rate() + samplesOut)
		samplesOutDuration = t.samplesOutDuration.rate()
		highestSent        = t.highestSentTimestampMetric.Get()
		highestRecv        = highestTimestamp.Get()
		// 接收到的最大的时间戳减去发送出去的最大的时间戳乘以进入的时间戳再乘以每秒进入的samples
		// samplesPending得到的其实是pending的samples的数目
		samplesPending     = (highestRecv - highestSent) * samplesIn * samplesKeptRatio
	)

	// We use an integral accumulator, like in a PID, to help dampen oscillation.
	t.integralAccumulator = t.integralAccumulator + (samplesPending * 0.1)

	if samplesOut <= 0 {
		return
	}

	var (
		// 平均每个sample的发送事件
		timePerSample = samplesOutDuration / samplesOut
		desiredShards = (timePerSample * samplesPending) / float64(time.Second)
	)
	level.Debug(t.logger).Log("msg", "QueueManager.caclulateDesiredShards",
		"samplesIn", samplesIn,
		"samplesOut", samplesOut,
		"samplesKeptRatio", samplesKeptRatio,
		"samplesPending", samplesPending,
		"samplesOutDuration", samplesOutDuration,
		"timePerSample", timePerSample,
		"desiredShards", desiredShards,
		"highestSent", highestSent,
		"highestRecv", highestRecv)

	// Changes in the number of shards must be greater than shardToleranceFraction.
	var (
		lowerBound = float64(t.numShards) * (1. - shardToleranceFraction)
		upperBound = float64(t.numShards) * (1. + shardToleranceFraction)
	)
	level.Debug(t.logger).Log("msg", "QueueManager.updateShardsLoop",
		"lowerBound", lowerBound, "desiredShards", desiredShards, "upperBound", upperBound)
	if lowerBound <= desiredShards && desiredShards <= upperBound {
		return
	}

	// 获取比desiredShards大的最小整数
	numShards := int(math.Ceil(desiredShards))
	if numShards > t.cfg.MaxShards {
		numShards = t.cfg.MaxShards
	} else if numShards < t.cfg.MinShards {
		numShards = t.cfg.MinShards
	}
	if numShards == t.numShards {
		return
	}

	// Resharding can take some time, and we want this loop
	// to stay close to shardUpdateDuration.
	// Resharding可能会耗费一些时间,如果当前正在resharding，则直接跳过
	select {
	case t.reshardChan <- numShards:
		level.Info(t.logger).Log("msg", "Remote storage resharding", "from", t.numShards, "to", numShards)
		t.numShards = numShards
	default:
		level.Info(t.logger).Log("msg", "Currently resharding, skipping.")
	}
}

func (t *QueueManager) reshardLoop() {
	defer t.wg.Done()

	for {
		select {
		case numShards := <-t.reshardChan:
			// We start the newShards after we have stopped (the therefore completely
			// flushed) the oldShards, to guarantee we only every deliver samples in
			// order.
			t.shards.stop()
			t.shards.start(numShards)
		case <-t.quit:
			return
		}
	}
}

func (t *QueueManager) newShards() *shards {
	s := &shards{
		qm:   t,
		done: make(chan struct{}),
	}
	return s
}

type shards struct {
	mtx sync.RWMutex // With the WAL, this is never actually contended.

	qm     *QueueManager
	queues []chan prompb.TimeSeries

	// Emulate a wait group with a channel and an atomic int, as you
	// cannot select on a wait group.
	done    chan struct{}
	running int32

	// Soft shutdown context will prevent new enqueues and deadlocks.
	// Soft shutdonw context会阻止新的enqueues以及死锁
	softShutdown chan struct{}

	// Hard shutdown context is used to terminate outgoing HTTP connections
	// after giving them a chance to terminate.
	// Hard shutdown context用来关闭对外的HTTP连接，在给了它们关闭的缓冲时间之后
	hardShutdown context.CancelFunc
}

// start the shards; must be called before any call to enqueue.
// 启动shards，必须在调用任何enqueue之前被调用
func (s *shards) start(n int) {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	newQueues := make([]chan prompb.TimeSeries, n)
	for i := 0; i < n; i++ {
		newQueues[i] = make(chan prompb.TimeSeries, s.qm.cfg.Capacity)
	}

	s.queues = newQueues

	var hardShutdownCtx context.Context
	hardShutdownCtx, s.hardShutdown = context.WithCancel(context.Background())
	s.softShutdown = make(chan struct{})
	s.running = int32(n)
	s.done = make(chan struct{})
	for i := 0; i < n; i++ {
		go s.runShard(hardShutdownCtx, i, newQueues[i])
	}
	s.qm.numShardsMetric.Set(float64(n))
}

// stop the shards; subsequent call to enqueue will return false.
func (s *shards) stop() {
	// Attempt a clean shutdown, but only wait flushDeadline for all the shards
	// to cleanly exit.  As we're doing RPCs, enqueue can block indefinitely.
	// We must be able so call stop concurrently, hence we can only take the
	// RLock here.
	s.mtx.RLock()
	close(s.softShutdown)
	s.mtx.RUnlock()

	// Enqueue should now be unblocked, so we can take the write lock.  This
	// also ensures we don't race with writes to the queues, and get a panic:
	// send on closed channel.
	s.mtx.Lock()
	defer s.mtx.Unlock()
	for _, queue := range s.queues {
		close(queue)
	}
	select {
	case <-s.done:
		return
	// 默认等待一分钟来shutdown
	case <-time.After(s.qm.flushDeadline):
		level.Error(s.qm.logger).Log("msg", "Failed to flush all samples on shutdown")
	}

	// Force an unclean shutdown.
	s.hardShutdown()
	<-s.done
}

// enqueue a sample.  If we are currently in the process of shutting down or resharding,
// will return false; in this case, you should back off and retry.
// 将一个sample入队，如果我们正处于关闭或者resharding的状态会返回false
// 在这种情况下，应该回退并且重试
func (s *shards) enqueue(ref uint64, sample prompb.TimeSeries) bool {
	// 入队的时候进行RLock()
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	select {
	case <-s.softShutdown:
		// 如果softShutdown已经关闭，则直接返回false
		return false
	default:
	}

	shard := uint64(ref) % uint64(len(s.queues))
	select {
	case <-s.softShutdown:
		return false
		// 如果队列满，则始终阻塞
	case s.queues[shard] <- sample:
		return true
	}
}

func (s *shards) runShard(ctx context.Context, i int, queue chan prompb.TimeSeries) {
	defer func() {
		if atomic.AddInt32(&s.running, -1) == 0 {
			close(s.done)
		}
	}()

	shardNum := strconv.Itoa(i)

	// Send batches of at most MaxSamplesPerSend samples to the remote storage.
	// If we have fewer samples than that, flush them out after a deadline
	// anyways.
	// MaxSamplesPerSend是100，如果我们的samples数目比这个少，则在deadline之后就flush
	max := s.qm.cfg.MaxSamplesPerSend
	pendingSamples := make([]prompb.TimeSeries, 0, max)
	var buf []byte

	timer := time.NewTimer(time.Duration(s.qm.cfg.BatchSendDeadline))
	stop := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	defer stop()

	for {
		select {
		case <-ctx.Done():
			return

		case sample, ok := <-queue:
			if !ok {
				if len(pendingSamples) > 0 {
					level.Debug(s.qm.logger).Log("msg", "Flushing samples to remote storage...", "count", len(pendingSamples))
					// 重用buf
					s.sendSamples(ctx, pendingSamples, &buf)
					s.qm.pendingSamplesMetric.Sub(float64(len(pendingSamples)))
					level.Debug(s.qm.logger).Log("msg", "Done flushing.")
				}
				return
			}

			// Number of pending samples is limited by the fact that sendSamples (via sendSamplesWithBackoff)
			// retries endlessly, so once we reach > 100 samples, if we can never send to the endpoint we'll
			// stop reading from the queue (which has a size of 10).
			pendingSamples = append(pendingSamples, sample)
			s.qm.pendingSamplesMetric.Inc()

			if len(pendingSamples) >= max {
				s.sendSamples(ctx, pendingSamples[:max], &buf)
				pendingSamples = append(pendingSamples[:0], pendingSamples[max:]...)
				s.qm.pendingSamplesMetric.Sub(float64(max))

				// 手动将定时时钟关闭
				stop()
				// BatchSendDeadline为100ms
				timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))
			}

		case <-timer.C:
			n := len(pendingSamples)
			if n > 0 {
				level.Debug(s.qm.logger).Log("msg", "runShard timer ticked, sending samples", "samples", n, "shard", shardNum)
				s.sendSamples(ctx, pendingSamples, &buf)
				pendingSamples = pendingSamples[:0]
				s.qm.pendingSamplesMetric.Sub(float64(n))
			}
			timer.Reset(time.Duration(s.qm.cfg.BatchSendDeadline))
		}
	}
}

func (s *shards) sendSamples(ctx context.Context, samples []prompb.TimeSeries, buf *[]byte) {
	begin := time.Now()
	err := s.sendSamplesWithBackoff(ctx, samples, buf)
	if err != nil {
		level.Error(s.qm.logger).Log("msg", "non-recoverable error", "count", len(samples), "err", err)
		// 通过回退也发送失败了，就记录为failed samples
		s.qm.failedSamplesTotal.Add(float64(len(samples)))
	}

	// These counters are used to calculate the dynamic sharding, and as such
	// should be maintained irrespective of success or failure.
	// 这些counters用来计算dynamic sharding，不论成功或者失败都应该被记录
	s.qm.samplesOut.incr(int64(len(samples)))
	s.qm.samplesOutDuration.incr(int64(time.Since(begin)))
}

// sendSamples to the remote storage with backoff for recoverable errors.
// sendSamples将samples写入远程存储，对于可恢复的错误进行backoff
func (s *shards) sendSamplesWithBackoff(ctx context.Context, samples []prompb.TimeSeries, buf *[]byte) error {
	backoff := s.qm.cfg.MinBackoff
	req, highest, err := buildWriteRequest(samples, *buf)
	*buf = req
	if err != nil {
		// Failing to build the write request is non-recoverable, since it will
		// only error if marshaling the proto to bytes fails.
		return err
	}

	// 如果是可恢复的错误，则始终进行重试
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		begin := time.Now()
		err := s.qm.client.Store(ctx, req)

		s.qm.sentBatchDuration.Observe(time.Since(begin).Seconds())

		if err == nil {
			s.qm.succeededSamplesTotal.Add(float64(len(samples)))
			s.qm.highestSentTimestampMetric.Set(float64(highest / 1000))
			return nil
		}

		// 如果不是可以恢复的错误，就直接返回
		if _, ok := err.(recoverableError); !ok {
			return err
		}
		s.qm.retriedSamplesTotal.Add(float64(len(samples)))
		level.Debug(s.qm.logger).Log("msg", "failed to send batch, retrying", "err", err)

		time.Sleep(time.Duration(backoff))
		backoff = backoff * 2
		if backoff > s.qm.cfg.MaxBackoff {
			backoff = s.qm.cfg.MaxBackoff
		}
	}
}

func buildWriteRequest(samples []prompb.TimeSeries, buf []byte) ([]byte, int64, error) {
	var highest int64
	for _, ts := range samples {
		// At the moment we only ever append a TimeSeries with a single sample in it.
		if ts.Samples[0].Timestamp > highest {
			// 记录最大的timestamp
			highest = ts.Samples[0].Timestamp
		}
	}
	req := &prompb.WriteRequest{
		Timeseries: samples,
	}

	data, err := proto.Marshal(req)
	if err != nil {
		return nil, highest, err
	}

	// snappy uses len() to see if it needs to allocate a new slice. Make the
	// buffer as long as possible.
	if buf != nil {
		buf = buf[0:cap(buf)]
	}
	compressed := snappy.Encode(buf, data)
	return compressed, highest, nil
}
