package jobworker

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Setting struct {
	Primary   Connector
	Secondary Connector

	DeadConnectorRetryInterval int64 // Seconds

	LoggerFunc LoggerFunc
}

var (
	ErrPrimaryConnIsRequired = errors.New("primary conn is required")
)

func New(s *Setting) (*JobWorker, error) {

	if s.Primary == nil {
		return nil, ErrPrimaryConnIsRequired
	}

	var w JobWorker
	w.connProvider.Register(1, s.Primary)
	if s.Secondary != nil {
		w.connProvider.Register(2, s.Secondary)
	}
	w.connProvider.SetRetrySeconds(time.Duration(s.DeadConnectorRetryInterval) * time.Second)
	w.loggerFunc = s.LoggerFunc

	return &w, nil
}

type JobWorker struct {
	connProvider ConnectorProvider

	queue2worker map[string]Worker

	loggerFunc LoggerFunc

	started int32

	inShutdown    int32
	mu            sync.Mutex
	activeJob     map[*Job]struct{}
	activeJobWg   sync.WaitGroup
	doneChan      chan struct{}
	cardiacArrest chan struct{}
	onShutdown    []func()
}

type LoggerFunc func(...interface{})

func (jw *JobWorker) EnqueueJob(ctx context.Context, input *EnqueueInput) error {

	for priority, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {

		if jw.connProvider.IsDead(conn) {
			jw.debug("connector is dead. priority: ", priority)
			continue
		}

		_, err := conn.Enqueue(ctx, input)
		if err != nil {

			if err == ErrJobDuplicationDetected {
				jw.debug("skip enqueue a duplication job")
				return nil
			}
			jw.debug("mark dead connector, because could not enqueue job. priority:", priority, "err:", err)
			jw.connProvider.MarkDead(conn)
			continue
		}
		return nil
	}

	return errors.New("could not enqueue a job using all connector")
}

func (jw *JobWorker) EnqueueJobBatch(ctx context.Context, input *EnqueueBatchInput) error {
	for priority, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {

		if jw.connProvider.IsDead(conn) {
			jw.debug("connector is dead. priority: ", priority)
			continue
		}

		output, err := conn.EnqueueBatch(ctx, input)

		if err == nil && output != nil && len(output.Failed) == 0 {
			return nil
		}

		jw.debug("could not enqueue job batch. priority: ", priority)
		jw.connProvider.MarkDead(conn)

		if output != nil && len(output.Failed) > 0 {
			for _, id := range output.Successful {
				delete(input.Id2Content, id)
			}
		}
	}

	return errors.New("could not enqueue batch some jobs using all connector")
}

type WorkerFunc func(job *Job) error

type Worker interface {
	Work(*Job) error
}

type defaultWorker struct {
	workFunc func(*Job) error
}

func (w *defaultWorker) Work(job *Job) error {
	return w.workFunc(job)
}

func (jw *JobWorker) RegisterFunc(queue string, f WorkerFunc) bool {
	return jw.Register(queue, &defaultWorker{
		workFunc: f,
	})
}

func (jw *JobWorker) Register(queue string, worker Worker) bool {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	if queue == "" || worker == nil {
		return false
	}
	if jw.queue2worker == nil {
		jw.queue2worker = make(map[string]Worker)
	}
	jw.queue2worker[queue] = worker
	return true
}

type WorkSetting struct {
	HeartbeatInterval     int64
	OnHeartBeat           func(job *Job)
	WorkerConcurrency     int
	Queue2PollingInterval map[string]int64 // key: queue name, value; polling interval (seconds)
}

const (
	workerConcurrencyDefault = 1
)

func (s *WorkSetting) setDefaults() {
	if s.WorkerConcurrency == 0 {
		s.WorkerConcurrency = workerConcurrencyDefault
	}
	if s.Queue2PollingInterval == nil {
		s.Queue2PollingInterval = make(map[string]int64)
	}
}

var (
	ErrAlreadyStarted        = errors.New("already started")
	ErrQueueSettingsRequired = errors.New("queue settings required")
)

type Broadcaster struct {
	mu *sync.Mutex
	c  *sync.Cond
}

func (b *Broadcaster) Register(operation func()) {
	if b.c == nil {
		b.mu = new(sync.Mutex)
		b.c = sync.NewCond(b.mu)
	}
	go func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.c.Wait()
		operation()
	}()
}

func (b *Broadcaster) Broadcast() {
	b.c.Broadcast()
}

func (jw *JobWorker) Work(s *WorkSetting) error {

	if atomic.LoadInt32(&jw.started) == 1 {
		return ErrAlreadyStarted
	}
	atomic.StoreInt32(&jw.started, 1)

	s.setDefaults()

	if len(s.Queue2PollingInterval) == 0 {
		return ErrQueueSettingsRequired
	}

	if s.HeartbeatInterval > 0 && s.OnHeartBeat != nil {
		interval := time.Duration(s.HeartbeatInterval) * time.Second
		jw.startHeartbeat(interval, s.OnHeartBeat)
	}

	var b Broadcaster
	go func() {
		<-jw.getDoneChan()
		b.Broadcast()
	}()

	b.Register(jw.stopHeartbeat)

	trackedJobCh := make(chan *Job)
	for _, conn := range jw.connProvider.GetConnectorsInPriorityOrder() {
		for name, interval := range s.Queue2PollingInterval {
			ctx := context.Background()
			output, err := conn.Subscribe(ctx, &SubscribeInput{
				Queue:    name,
				Interval: time.Duration(interval) * time.Second,
			})
			if err != nil {
				return err
			}
			b.Register(func() {
				err := output.Subscription.UnSubscribe()
				if err != nil {
					jw.debug("an error occurred during unsubscribe:", name, err)
				}
			})

			go func(sub Subscription) {
				for job := range sub.Queue() {
					trackedJobCh <- job
					jw.trackJob(job, true)
				}
			}(output.Subscription)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < s.WorkerConcurrency; i++ {
		wg.Add(1)
		go func(id int) {
			sw := subWorker{id: strconv.Itoa(id), JobWorker: jw}
			sw.work(trackedJobCh)
			wg.Done()
		}(i)
	}

	wg.Wait()
	close(trackedJobCh)

	return nil

}

type subWorker struct {
	id string
	*JobWorker
}

func (sw *subWorker) work(jobs <-chan *Job) {
	for job := range jobs {
		sw.workSafely(context.Background(), job)
	}
}

func (jw *JobWorker) workSafely(ctx context.Context, job *Job) {

	conn := job.conn
	queue := job.Queue()
	payload := job.Payload()

	jw.debug("start work safely:", conn.Name(), queue, payload.Content)

	defer jw.trackJob(job, false)

	jw.trackJob(job, true)
	defer jw.trackJob(job, false)

	w, ok := jw.queue2worker[queue]
	if !ok {
		jw.debug("could not found queue:", queue)
		return
	}

	if err := w.Work(job); err != nil {
		if err = failJob(ctx, job); err != nil {
			jw.debug("mark dead connector, because error occurred during job fail:",
				conn.Name(), queue, payload.Content, err)
			jw.connProvider.MarkDead(conn)
		}
		return
	}
	if err := completeJob(ctx, job); err != nil {
		jw.debug("mark dead connector, because error occurred during job complete:",
			conn.Name(), queue, payload.Content, err)
		jw.connProvider.MarkDead(conn)
		return
	}
	jw.debug("success work safely:", conn.Name(), queue, payload.Content)
}

func (jw *JobWorker) RegisterOnShutdown(f func()) {
	jw.mu.Lock()
	jw.onShutdown = append(jw.onShutdown, f)
	jw.mu.Unlock()
}

func (jw *JobWorker) Shutdown(ctx context.Context) error {
	atomic.StoreInt32(&jw.inShutdown, 1)

	jw.mu.Lock()
	jw.closeDoneChanLocked()
	for _, f := range jw.onShutdown {
		go f()
	}
	jw.mu.Unlock()

	finished := make(chan struct{}, 1)
	go func() {
		jw.activeJobWg.Wait()
		finished <- struct{}{}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-finished:
		return nil
	}
}

const logPrefix = "[JWDK]"

func (jw *JobWorker) debug(args ...interface{}) {
	if jw.verbose() {
		args = append([]interface{}{logPrefix}, args...)
		jw.loggerFunc(args...)
	}
}

func (jw *JobWorker) verbose() bool {
	return jw.loggerFunc != nil
}

func (jw *JobWorker) startHeartbeat(interval time.Duration, f func(job *Job)) {
	jw.debug("start heart beat - interval:", interval)
	go func() {
		for {
			select {
			case <-jw.cardiacArrest:
				return
			default:
				var jobs []*Job
				jw.mu.Lock()
				for v := range jw.activeJob {
					jobs = append(jobs, v)
				}
				jw.mu.Unlock()

				go func(jobs []*Job) {
					for _, job := range jobs {
						f(job)
					}
				}(jobs)

			}
			time.Sleep(interval)
		}
	}()
}

func (jw *JobWorker) stopHeartbeat() {
	jw.debug("stop heart beat")
	jw.cardiacArrest <- struct{}{}
}

func (jw *JobWorker) shuttingDown() bool {
	return atomic.LoadInt32(&jw.inShutdown) != 0
}

func (jw *JobWorker) trackJob(job *Job, add bool) {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	if jw.activeJob == nil {
		jw.activeJob = make(map[*Job]struct{})
	}
	if add {
		jw.activeJob[job] = struct{}{}
		jw.activeJobWg.Add(1)
	} else {
		delete(jw.activeJob, job)
		jw.activeJobWg.Done()
	}
	jw.debug("active job size:", len(jw.activeJob))
}

func (jw *JobWorker) getDoneChan() <-chan struct{} {
	jw.mu.Lock()
	defer jw.mu.Unlock()
	return jw.getDoneChanLocked()
}

func (jw *JobWorker) getDoneChanLocked() chan struct{} {
	if jw.doneChan == nil {
		jw.doneChan = make(chan struct{})
	}
	return jw.doneChan
}

func (jw *JobWorker) closeDoneChanLocked() {
	ch := jw.getDoneChanLocked()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func completeJob(ctx context.Context, job *Job) error {
	if job.IsFinished() {
		return nil
	}
	_, err := job.conn.CompleteJob(ctx, &CompleteJobInput{Job: job})
	if err != nil {
		return err
	}
	job.finished()
	return nil
}

func failJob(ctx context.Context, job *Job) error {
	if job.IsFinished() {
		return nil
	}
	_, err := job.conn.FailJob(ctx, &FailJobInput{Job: job})
	if err != nil {
		return err
	}
	job.finished()
	return nil
}
