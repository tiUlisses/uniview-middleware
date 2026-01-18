package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clientpkg "uniview-middleware/pkg/uniview/client"
	"uniview-middleware/pkg/uniview/receiver"
)

func main() {
	logLevel := getenv("LOG_LEVEL", "info")
	logger := log.New(os.Stdout, "[univiewd] ", log.LstdFlags)
	logger.Printf("log level: %s", logLevel)

	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}

	subcommand := os.Args[1]
	switch subcommand {
	case "serve":
		runServe(logger)
	case "subscribe":
		runSubscribe(logger)
	case "keepalive":
		runKeepAlive(logger)
	case "unsubscribe":
		runUnsubscribe(logger)
	case "run":
		runDaemon(logger)
	default:
		printUsage()
		os.Exit(1)
	}
}

func runServe(logger *log.Logger) {
	host := getenv("RECEIVER_HOST", "0.0.0.0")
	port := getenvInt("RECEIVER_PORT", 8080)

	logger.Printf("starting receiver on %s:%d", host, port)
	handler := receiver.HandlerFunc(func(ctx context.Context, event receiver.Event) error {
		logger.Printf("event received path=%s alarm=%s bytes=%d", event.Path, event.AlarmType, len(event.Raw))
		return nil
	})
	forwardHandler, err := newForwardingHandler(logger)
	if err != nil {
		logger.Printf("forwarding disabled: %v", err)
	} else {
		handler = forwardHandler
	}

	receiver, err := receiver.New(host, port, handler, logger)
	if err != nil {
		logger.Fatalf("receiver init: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := receiver.Start(); err != nil {
			logger.Fatalf("receiver error: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiver.Shutdown(shutdownCtx); err != nil {
		logger.Printf("receiver shutdown error: %v", err)
	}
}

func runSubscribe(logger *log.Logger) {
	cfg := loadConfig()
	cl := mustClient(cfg, logger)
	payload, err := loadPayload(cfg, "SUBSCRIBE_PAYLOAD", "SUBSCRIBE_PAYLOAD_FILE")
	if err != nil {
		logger.Fatalf("subscribe payload: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, resp, err := cl.Subscribe(ctx, clientpkg.SubscribeRequest{Payload: payload})
	if err != nil {
		logger.Fatalf("subscribe failed: %v", err)
	}
	logger.Printf("subscription created id=%s status=%d", id, resp.StatusCode)
}

func runKeepAlive(logger *log.Logger) {
	cfg := loadConfig()
	cl := mustClient(cfg, logger)
	subID := getenv("SUBSCRIPTION_ID", "")
	if subID == "" {
		logger.Fatalf("SUBSCRIPTION_ID is required")
	}
	payload, err := loadPayload(cfg, "KEEPALIVE_PAYLOAD", "KEEPALIVE_PAYLOAD_FILE")
	if err != nil {
		logger.Fatalf("keepalive payload: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cl.KeepAlive(ctx, subID, clientpkg.KeepAliveRequest{Payload: payload})
	if err != nil {
		logger.Fatalf("keepalive failed: %v", err)
	}
	logger.Printf("keepalive ok status=%d", resp.StatusCode)
}

func runUnsubscribe(logger *log.Logger) {
	cfg := loadConfig()
	cl := mustClient(cfg, logger)
	subID := getenv("SUBSCRIPTION_ID", "")
	if subID == "" {
		logger.Fatalf("SUBSCRIPTION_ID is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cl.Unsubscribe(ctx, subID)
	if err != nil {
		logger.Fatalf("unsubscribe failed: %v", err)
	}
	logger.Printf("unsubscribe ok status=%d", resp.StatusCode)
}

func runDaemon(logger *log.Logger) {
	cfg := loadConfig()
	cl := mustClient(cfg, logger)
	host := getenv("RECEIVER_HOST", "0.0.0.0")
	port := getenvInt("RECEIVER_PORT", 8080)

	handler := receiver.HandlerFunc(func(ctx context.Context, event receiver.Event) error {
		logger.Printf("event received path=%s alarm=%s bytes=%d", event.Path, event.AlarmType, len(event.Raw))
		return nil
	})
	forwardHandler, err := newForwardingHandler(logger)
	if err != nil {
		logger.Printf("forwarding disabled: %v", err)
	} else {
		handler = forwardHandler
	}

	receiverSrv, err := receiver.New(host, port, handler, logger)
	if err != nil {
		logger.Fatalf("receiver init: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := receiverSrv.Start(); err != nil {
			logger.Fatalf("receiver error: %v", err)
		}
	}()

	subscribePayload, err := loadPayload(cfg, "SUBSCRIBE_PAYLOAD", "SUBSCRIBE_PAYLOAD_FILE")
	if err != nil {
		logger.Fatalf("subscribe payload: %v", err)
	}
	subCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	subID, resp, err := cl.Subscribe(subCtx, clientpkg.SubscribeRequest{Payload: subscribePayload})
	cancel()
	if err != nil {
		logger.Fatalf("subscribe failed: %v", err)
	}
	logger.Printf("subscription created id=%s status=%d", subID, resp.StatusCode)

	ticker := time.NewTicker(cfg.KeepAliveInterval())
	defer ticker.Stop()

	go func() {
		for {
			select {
			case <-ticker.C:
				payload, err := loadPayload(cfg, "KEEPALIVE_PAYLOAD", "KEEPALIVE_PAYLOAD_FILE")
				if err != nil {
					logger.Printf("keepalive payload error: %v", err)
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err = cl.KeepAlive(ctx, subID, clientpkg.KeepAliveRequest{Payload: payload})
				cancel()
				if err != nil {
					logger.Printf("keepalive failed: %v", err)
				} else {
					logger.Printf("keepalive ok")
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiverSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("receiver shutdown error: %v", err)
	}
}

type config struct {
	BaseURL       string
	User          string
	Pass          string
	ReceiverHost  string
	ReceiverPort  int
	Duration      int
	TypeMask      int
	ImagePushMode int
}

func loadConfig() config {
	return config{
		BaseURL:       getenv("UNV_BASE_URL", ""),
		User:          getenv("UNV_USER", ""),
		Pass:          getenv("UNV_PASS", ""),
		ReceiverHost:  getenv("RECEIVER_HOST", "0.0.0.0"),
		ReceiverPort:  getenvInt("RECEIVER_PORT", 8080),
		Duration:      getenvInt("DURATION", 60),
		TypeMask:      getenvInt("TYPE_MASK", 0),
		ImagePushMode: getenvInt("IMAGE_PUSH_MODE", 0),
	}
}

func (c config) CallbackURL() string {
	return fmt.Sprintf("http://%s:%d/LAPI/V1.0/System/Event/Notification", c.ReceiverHost, c.ReceiverPort)
}

func (c config) KeepAliveInterval() time.Duration {
	if c.Duration <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.Duration/2) * time.Second
}

func mustClient(cfg config, logger *log.Logger) *clientpkg.Client {
	if cfg.BaseURL == "" || cfg.User == "" || cfg.Pass == "" {
		logger.Fatalf("UNV_BASE_URL, UNV_USER, and UNV_PASS are required")
	}
	client, err := clientpkg.NewClient(cfg.BaseURL, cfg.User, cfg.Pass, nil)
	if err != nil {
		logger.Fatalf("client init: %v", err)
	}
	return client
}

func printUsage() {
	fmt.Println("Usage: univiewd <serve|subscribe|keepalive|unsubscribe|run>")
}

func loadPayload(cfg config, envKey, fileKey string) ([]byte, error) {
	value := os.Getenv(envKey)
	if value == "" {
		filePath := os.Getenv(fileKey)
		if filePath != "" {
			data, err := os.ReadFile(filePath)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", fileKey, err)
			}
			value = string(data)
		}
	}
	if value == "" {
		return nil, fmt.Errorf("missing payload: set %s or %s", envKey, fileKey)
	}
	rendered := renderTemplate(value, cfg)
	return []byte(rendered), nil
}

func renderTemplate(template string, cfg config) string {
	replacements := map[string]string{
		"{{CALLBACK_URL}}":    cfg.CallbackURL(),
		"{{DURATION}}":        strconv.Itoa(cfg.Duration),
		"{{TYPE_MASK}}":       strconv.Itoa(cfg.TypeMask),
		"{{IMAGE_PUSH_MODE}}": strconv.Itoa(cfg.ImagePushMode),
		"{{SUBSCRIPTION_ID}}": os.Getenv("SUBSCRIPTION_ID"),
	}
	output := template
	for key, value := range replacements {
		output = strings.ReplaceAll(output, key, value)
	}
	return output
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
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

type forwardPayload struct {
	Path       string          `json:"path"`
	AlarmType  string          `json:"alarm_type"`
	ReceivedAt string          `json:"received_at"`
	Headers    http.Header     `json:"headers"`
	Event      json.RawMessage `json:"event"`
}

func newForwardingHandler(logger *log.Logger) (receiver.HandlerFunc, error) {
	forwardURL, err := receiver.ForwardURLFromEnv()
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, event receiver.Event) error {
		logger.Printf("event received path=%s alarm=%s bytes=%d", event.Path, event.AlarmType, len(event.Raw))
		payload := forwardPayload{
			Path:       event.Path,
			AlarmType:  event.AlarmType,
			ReceivedAt: event.ReceivedAt.Format(time.RFC3339Nano),
			Headers:    event.Headers,
			Event:      json.RawMessage(event.Raw),
		}
		body, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal forward payload: %w", err)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, forwardURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create forward request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("forward event: %w", err)
		}
		defer resp.Body.Close()
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			logger.Printf("forward response read error: %v", err)
		}
		if resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("forward event status=%d", resp.StatusCode)
		}
		return nil
	}, nil
}
