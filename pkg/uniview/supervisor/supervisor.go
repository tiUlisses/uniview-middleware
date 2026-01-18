package supervisor

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	clientpkg "uniview-middleware/pkg/uniview/client"
)

const (
	defaultMaxConcurrency    = 4
	defaultSubscribeBackoff  = 15 * time.Second
	defaultKeepAliveJitter   = 2 * time.Second
	defaultWorkerShutdown    = 10 * time.Second
	defaultKeepAliveFailures = 3
	defaultOperationTimeout  = 10 * time.Second
	defaultKeepAliveBackoff  = 2 * time.Second
	defaultKeepAliveMaxBack  = 30 * time.Second
)

type CameraConfig struct {
	BaseURL string
	User    string
	Pass    string
	Label   string
	Model   string
}

type Config struct {
	MaxConcurrency        int
	SubscribeRetryBackoff time.Duration
	KeepAliveJitter       time.Duration
	WorkerShutdownTimeout time.Duration
	MaxKeepAliveFailures  int
	KeepAliveBackoffBase  time.Duration
	KeepAliveBackoffMax   time.Duration
}

type PayloadProvider interface {
	SubscribePayload(camera CameraConfig) ([]byte, error)
	KeepAlivePayload(camera CameraConfig) ([]byte, error)
}

type Supervisor struct {
	logger            *log.Logger
	config            Config
	keepAliveInterval time.Duration
	payloads          PayloadProvider
	cameraLoader      func() ([]CameraConfig, error)
}

type Worker struct {
	ctx                 context.Context
	cancel              context.CancelFunc
	camera              CameraConfig
	logger              *log.Logger
	config              Config
	payloads            PayloadProvider
	keepAliveInterval   time.Duration
	SubID               string
	LastFailure         time.Time
	NextAttempt         time.Time
	consecutiveFailures int
	rng                 *rand.Rand
}

func LoadConfigFromEnv() Config {
	return Config{
		MaxConcurrency:        getenvInt("WORKER_MAX_CONCURRENCY", defaultMaxConcurrency),
		SubscribeRetryBackoff: getenvDuration("SUBSCRIBE_RETRY_BACKOFF", defaultSubscribeBackoff),
		KeepAliveJitter:       getenvDuration("KEEPALIVE_JITTER", defaultKeepAliveJitter),
		WorkerShutdownTimeout: getenvDuration("WORKER_SHUTDOWN_TIMEOUT", defaultWorkerShutdown),
		MaxKeepAliveFailures:  getenvIntFromKeys([]string{"MAX_KEEPALIVE_FAILURES", "KEEPALIVE_MAX_FAILURES"}, defaultKeepAliveFailures),
		KeepAliveBackoffBase:  getenvDuration("KEEPALIVE_BACKOFF_BASE", defaultKeepAliveBackoff),
		KeepAliveBackoffMax:   getenvDuration("KEEPALIVE_BACKOFF_MAX", defaultKeepAliveMaxBack),
	}
}

func New(logger *log.Logger, cfg Config, keepAliveInterval time.Duration, payloads PayloadProvider, cameraLoader func() ([]CameraConfig, error)) *Supervisor {
	return &Supervisor{
		logger:            logger,
		config:            cfg,
		keepAliveInterval: keepAliveInterval,
		payloads:          payloads,
		cameraLoader:      cameraLoader,
	}
}

func (s *Supervisor) Run(ctx context.Context) error {
	cameras, err := s.cameraLoader()
	if err != nil {
		return fmt.Errorf("load camera configs: %w", err)
	}
	if len(cameras) == 0 {
		return fmt.Errorf("no cameras configured")
	}
	s.logger.Printf("supervisor initialized for %d camera(s)", len(cameras))

	maxConcurrency := s.config.MaxConcurrency
	if maxConcurrency <= 0 {
		maxConcurrency = len(cameras)
	}
	semaphore := make(chan struct{}, maxConcurrency)
	var wg sync.WaitGroup
	for _, cam := range cameras {
		camera := cam
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runWorkerLoop(ctx, camera, semaphore)
		}()
	}

	<-ctx.Done()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	shutdownTimeout := s.config.WorkerShutdownTimeout
	if shutdownTimeout <= 0 {
		shutdownTimeout = defaultWorkerShutdown
	}
	select {
	case <-done:
		return nil
	case <-time.After(shutdownTimeout):
		return fmt.Errorf("worker shutdown timeout after %s", shutdownTimeout)
	}
}

func (s *Supervisor) runWorkerLoop(ctx context.Context, camera CameraConfig, semaphore chan struct{}) {
	for {
		select {
		case <-ctx.Done():
			return
		case semaphore <- struct{}{}:
		}

		worker := NewWorker(ctx, camera, s.logger, s.config, s.payloads, s.keepAliveInterval)
		err := worker.Run()
		<-semaphore

		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("worker stopped without error")
		}
		worker.LastFailure = time.Now()
		worker.NextAttempt = worker.LastFailure.Add(s.config.SubscribeRetryBackoff)
		s.logger.Printf("camera %s worker stopped: %v (retry at %s)", camera.Label, err, worker.NextAttempt.Format(time.RFC3339))

		wait := time.Until(worker.NextAttempt)
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func NewWorker(ctx context.Context, camera CameraConfig, logger *log.Logger, cfg Config, payloads PayloadProvider, keepAliveInterval time.Duration) *Worker {
	workerCtx, cancel := context.WithCancel(ctx)
	return &Worker{
		ctx:               workerCtx,
		cancel:            cancel,
		camera:            camera,
		logger:            logger,
		config:            cfg,
		payloads:          payloads,
		keepAliveInterval: keepAliveInterval,
		rng:               rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (w *Worker) Run() error {
	defer w.cancel()
	client, err := clientpkg.NewClient(w.camera.BaseURL, w.camera.User, w.camera.Pass, nil)
	if err != nil {
		return fmt.Errorf("client init: %w", err)
	}
	defer w.unsubscribe(client)

	if err := w.subscribe(client); err != nil {
		w.LastFailure = time.Now()
		return err
	}
	return w.keepAliveLoop(client)
}

func (w *Worker) subscribe(client *clientpkg.Client) error {
	payload, err := w.payloads.SubscribePayload(w.camera)
	if err != nil {
		return fmt.Errorf("subscribe payload: %w", err)
	}
	subCtx, cancel := context.WithTimeout(w.ctx, defaultOperationTimeout)
	defer cancel()
	subID, resp, err := client.Subscribe(subCtx, clientpkg.SubscribeRequest{Payload: payload})
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	w.SubID = subID
	w.logger.Printf("camera %s subscription created id=%s status=%d model=%s", w.camera.Label, subID, resp.StatusCode, w.camera.Model)
	return nil
}

func (w *Worker) keepAliveLoop(client *clientpkg.Client) error {
	failuresAllowed := w.config.MaxKeepAliveFailures
	if failuresAllowed <= 0 {
		failuresAllowed = defaultKeepAliveFailures
	}

	for {
		wait := w.nextKeepAliveDelay()
		timer := time.NewTimer(wait)
		select {
		case <-w.ctx.Done():
			timer.Stop()
			return w.ctx.Err()
		case <-timer.C:
		}

		payload, err := w.payloads.KeepAlivePayload(w.camera)
		if err != nil {
			w.incrementFailure()
			w.logger.Printf("event=keepalive_payload_error camera_label=%s camera_ip=%s attempts=%d error=%v", w.camera.Label, w.cameraHost(), w.consecutiveFailures, err)
			if w.consecutiveFailures >= failuresAllowed {
				if err := w.resetSubscription(client); err != nil {
					return err
				}
			}
			continue
		}

		kaCtx, cancel := context.WithTimeout(w.ctx, defaultOperationTimeout)
		resp, err := client.KeepAlive(kaCtx, w.SubID, clientpkg.KeepAliveRequest{Payload: payload})
		cancel()
		if err != nil {
			w.incrementFailure()
			w.logger.Printf("event=keepalive_failed camera_label=%s camera_ip=%s sub_id=%s attempts=%d error=%v", w.camera.Label, w.cameraHost(), w.SubID, w.consecutiveFailures, err)
			if w.consecutiveFailures >= failuresAllowed {
				if err := w.resetSubscription(client); err != nil {
					return err
				}
			}
			continue
		}
		w.consecutiveFailures = 0
		w.logger.Printf("event=keepalive_ok camera_label=%s camera_ip=%s sub_id=%s status=%d attempts=%d", w.camera.Label, w.cameraHost(), w.SubID, resp.StatusCode, w.consecutiveFailures)
	}
}

func (w *Worker) unsubscribe(client *clientpkg.Client) {
	if w.SubID == "" {
		return
	}
	timeout := w.config.WorkerShutdownTimeout
	if timeout <= 0 {
		timeout = defaultWorkerShutdown
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if _, err := client.Unsubscribe(ctx, w.SubID); err != nil {
		w.logger.Printf("camera %s unsubscribe failed id=%s error=%v", w.camera.Label, w.SubID, err)
	} else {
		w.logger.Printf("camera %s unsubscribed id=%s", w.camera.Label, w.SubID)
	}
	w.SubID = ""
}

func (w *Worker) nextKeepAliveInterval() time.Duration {
	base := w.keepAliveInterval
	if base <= 0 {
		base = 30 * time.Second
	}
	jitter := w.config.KeepAliveJitter
	if jitter <= 0 {
		return base
	}
	return base + time.Duration(w.rng.Int63n(int64(jitter)))
}

func (w *Worker) nextKeepAliveDelay() time.Duration {
	if w.consecutiveFailures > 0 {
		return w.nextKeepAliveBackoff()
	}
	return w.nextKeepAliveInterval()
}

func (w *Worker) nextKeepAliveBackoff() time.Duration {
	base := w.config.KeepAliveBackoffBase
	if base <= 0 {
		base = defaultKeepAliveBackoff
	}
	maxBackoff := w.config.KeepAliveBackoffMax
	if maxBackoff <= 0 {
		maxBackoff = defaultKeepAliveMaxBack
	}
	backoff := base
	for i := 1; i < w.consecutiveFailures; i++ {
		if backoff >= maxBackoff/2 {
			backoff = maxBackoff
			break
		}
		backoff *= 2
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if backoff <= 0 {
		return 0
	}
	return time.Duration(w.rng.Int63n(int64(backoff) + 1))
}

func (w *Worker) incrementFailure() {
	w.consecutiveFailures++
	w.LastFailure = time.Now()
	w.NextAttempt = w.LastFailure.Add(w.config.SubscribeRetryBackoff)
}

func (w *Worker) resetSubscription(client *clientpkg.Client) error {
	w.logger.Printf("event=keepalive_resubscribe camera_label=%s camera_ip=%s attempts=%d", w.camera.Label, w.cameraHost(), w.consecutiveFailures)
	if w.SubID != "" {
		w.unsubscribe(client)
	}
	if err := w.subscribe(client); err != nil {
		return fmt.Errorf("keepalive resubscribe: %w", err)
	}
	w.consecutiveFailures = 0
	return nil
}

func (w *Worker) cameraHost() string {
	if w.camera.BaseURL == "" {
		return ""
	}
	parsed, err := url.Parse(w.camera.BaseURL)
	if err != nil {
		return w.camera.BaseURL
	}
	if parsed.Hostname() != "" {
		return parsed.Hostname()
	}
	return parsed.Host
}

func getenvInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvIntFromKeys(keys []string, fallback int) int {
	for _, key := range keys {
		value := strings.TrimSpace(os.Getenv(key))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err == nil {
			return parsed
		}
	}
	return fallback
}

func getenvDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	if parsed, err := time.ParseDuration(value); err == nil {
		return parsed
	}
	parsedInt, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return time.Duration(parsedInt) * time.Second
}
