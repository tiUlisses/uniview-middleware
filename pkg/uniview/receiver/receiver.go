package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultReadTimeout  = 10 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

var (
	metricEventsTotal   = expvar.NewInt("uniview_events_total")
	metricEventsFailed  = expvar.NewInt("uniview_events_failed")
	metricAckErrors     = expvar.NewInt("uniview_ack_errors")
	metricRequestsTotal = expvar.NewInt("uniview_requests_total")
)

// EventHandler handles incoming event notifications.
type EventHandler interface {
	HandleEvent(ctx context.Context, event Event) error
}

// HandlerFunc adapts a function to the EventHandler interface.
type HandlerFunc func(ctx context.Context, event Event) error

func (f HandlerFunc) HandleEvent(ctx context.Context, event Event) error {
	return f(ctx, event)
}

// ForwardingHandler posts the raw event payload to a configured endpoint.
type ForwardingHandler struct {
	client *http.Client
	url    string
	logger *log.Logger
}

// NewForwardingHandler builds a forwarding handler for the given URL.
func NewForwardingHandler(url string, client *http.Client, logger *log.Logger) *ForwardingHandler {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if logger == nil {
		logger = log.Default()
	}
	return &ForwardingHandler{client: client, url: url, logger: logger}
}

// NewEnvForwardingHandler builds a forwarding handler using env-configured settings.
func NewEnvForwardingHandler(logger *log.Logger) (EventHandler, error) {
	forwardURL, err := ForwardURLFromEnv()
	if err != nil {
		return nil, err
	}
	return NewForwardingHandler(forwardURL, nil, logger), nil
}

// ForwardURLFromEnv resolves the forwarding URL from environment variables.
func ForwardURLFromEnv() (string, error) {
	if value := strings.TrimSpace(os.Getenv("FORWARD_URL")); value != "" {
		return value, nil
	}

	host := strings.TrimSpace(os.Getenv("FORWARD_HOST"))
	if host == "" {
		return "", errors.New("missing FORWARD_URL or FORWARD_HOST")
	}
	scheme := strings.TrimSpace(os.Getenv("FORWARD_SCHEME"))
	if scheme == "" {
		scheme = "http"
	}
	path := strings.TrimSpace(os.Getenv("FORWARD_PATH"))
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	if port := strings.TrimSpace(os.Getenv("FORWARD_PORT")); port != "" {
		if _, err := strconv.Atoi(port); err != nil {
			return "", fmt.Errorf("invalid FORWARD_PORT: %w", err)
		}
		host = net.JoinHostPort(host, port)
	}

	forwardURL := url.URL{
		Scheme: scheme,
		Host:   host,
		Path:   path,
	}
	return forwardURL.String(), nil
}

// HandleEvent forwards the event payload to the configured endpoint.
func (h *ForwardingHandler) HandleEvent(ctx context.Context, event Event) error {
	if h == nil {
		return errors.New("forwarding handler is nil")
	}
	if h.url == "" {
		return errors.New("forwarding URL is empty")
	}
	contentType := event.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(event.Raw))
	if err != nil {
		return fmt.Errorf("create forward request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("forward event: %w", err)
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		h.logger.Printf("forward response read error: %v", err)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("forward event status=%d", resp.StatusCode)
	}
	return nil
}

// Event represents a notification payload.
type Event struct {
	Path       string
	Raw        json.RawMessage
	Headers    http.Header
	ReceivedAt time.Time
	AlarmType  string
}

// Receiver serves HTTP notifications from Uniview cameras.
type Receiver struct {
	addr       string
	server     *http.Server
	logger     *log.Logger
	handler    EventHandler
	startOnce  sync.Once
	stopOnce   sync.Once
	prefixPath string
}

// New creates a Receiver listening on the given host/port.
func New(host string, port int, handler EventHandler, logger *log.Logger) (*Receiver, error) {
	if handler == nil {
		return nil, errors.New("event handler is required")
	}
	if logger == nil {
		logger = log.Default()
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	return &Receiver{
		addr:       addr,
		logger:     logger,
		handler:    handler,
		prefixPath: "/LAPI/V1.0/System/Event/Notification",
	}, nil
}

// Start begins serving HTTP requests.
func (r *Receiver) Start() error {
	var err error
	r.startOnce.Do(func() {
		mux := http.NewServeMux()
		mux.HandleFunc(r.prefixPath+"/", r.handleNotification)
		mux.HandleFunc(r.prefixPath, r.handleNotification)
		mux.Handle("/debug/vars", expvar.Handler())
		r.server = &http.Server{
			Addr:         r.addr,
			Handler:      mux,
			ReadTimeout:  defaultReadTimeout,
			WriteTimeout: defaultWriteTimeout,
		}
		err = r.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
	})
	return err
}

// Shutdown stops the receiver.
func (r *Receiver) Shutdown(ctx context.Context) error {
	var err error
	r.stopOnce.Do(func() {
		if r.server == nil {
			return
		}
		err = r.server.Shutdown(ctx)
	})
	return err
}

func (r *Receiver) handleNotification(w http.ResponseWriter, req *http.Request) {
	metricRequestsTotal.Add(1)
	defer req.Body.Close()

	body, err := io.ReadAll(req.Body)
	if err != nil {
		metricEventsFailed.Add(1)
		r.logger.Printf("read notification body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	event := Event{
		Path:       req.URL.Path,
		Raw:        body,
		Headers:    req.Header.Clone(),
		ReceivedAt: time.Now(),
		AlarmType:  extractAlarmType(body),
	}

	if err := r.handler.HandleEvent(req.Context(), event); err != nil {
		metricEventsFailed.Add(1)
		r.logger.Printf("handle event error: %v", err)
	}
	metricEventsTotal.Add(1)

	ack := map[string]any{
		"ResponseCode": 0,
		"ResponseURL":  req.URL.Path,
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		metricAckErrors.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(payload); err != nil {
		r.logger.Printf("write ack: %v", err)
	}
}

func extractAlarmType(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	if value, ok := payload["AlarmType"]; ok {
		if alarm, ok := value.(string); ok {
			return alarm
		}
	}
	if value, ok := payload["alarmType"]; ok {
		if alarm, ok := value.(string); ok {
			return alarm
		}
	}
	for key, value := range payload {
		if strings.EqualFold(key, "AlarmType") {
			if alarm, ok := value.(string); ok {
				return alarm
			}
		}
	}
	return ""
}
