package main

import (
	"bufio"
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
	if err := loadDotEnv(); err != nil {
		log.Printf("failed to load .env: %v", err)
	}
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

func loadDotEnv() error {
	file, err := os.Open(".env")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export"))
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
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

	cameraConfigs, err := loadCameraConfigs(cfg)
	if err != nil {
		logger.Fatalf("camera config: %v", err)
	}
	logger.Printf("starting subscriptions for %d camera(s)", len(cameraConfigs))

	subscriptions := make(map[string]*cameraSubscription, len(cameraConfigs))
	for _, cam := range cameraConfigs {
		sub, err := subscribeCamera(ctx, logger, cfg, cam)
		if err != nil {
			logger.Printf("camera %s subscribe failed: %v", cam.Label, err)
			continue
		}
		subscriptions[cam.Label] = sub
		go runCameraKeepAlive(ctx, logger, cfg, sub)
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiverSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("receiver shutdown error: %v", err)
	}
}

type cameraConfig struct {
	BaseURL string
	User    string
	Pass    string
	Label   string
	Model   string
}

type cameraSubscription struct {
	camera cameraConfig
	client *clientpkg.Client
	subID  string
	ticker *time.Ticker
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

func newForwardingHandler(logger *log.Logger) (receiver.HandlerFunc, error) {
	forwardURL, err := receiver.ForwardURLFromEnv()
	if err != nil {
		return nil, err
	}
	mappings, err := receiver.LoadAlarmTypeMappingsFromEnv()
	if err != nil {
		return nil, err
	}
	config := receiver.NormalizationConfig{
		Tag:               receiver.EventTagFromEnv(),
		Category:          receiver.EventCategoryFromEnv(),
		AlarmTypeMappings: mappings,
	}
	client := &http.Client{Timeout: 10 * time.Second}
	return func(ctx context.Context, event receiver.Event) error {
		logger.Printf("event received path=%s alarm=%s camera_ip=%s bytes=%d", event.Path, event.AlarmType, event.CameraIP, len(event.Raw))
		payload := receiver.BuildNormalizedPayload(event, config)
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

func loadCameraConfigs(cfg config) ([]cameraConfig, error) {
	csvPath := getenv("CAMERA_CSV_FILE", "")
	if csvPath == "" {
		if cfg.BaseURL == "" || cfg.User == "" || cfg.Pass == "" {
			return nil, fmt.Errorf("UNV_BASE_URL, UNV_USER, and UNV_PASS are required")
		}
		return []cameraConfig{{
			BaseURL: cfg.BaseURL,
			User:    cfg.User,
			Pass:    cfg.Pass,
			Label:   cfg.BaseURL,
			Model:   "uniview",
		}}, nil
	}

	file, err := os.Open(csvPath)
	if err != nil {
		return nil, fmt.Errorf("open CAMERA_CSV_FILE: %w", err)
	}
	defer file.Close()
	entries, err := clientpkg.ParseCameraCSV(file)
	if err != nil {
		return nil, fmt.Errorf("parse CAMERA_CSV_FILE: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("CAMERA_CSV_FILE is empty")
	}
	cameras := make([]cameraConfig, 0, len(entries))
	for _, entry := range entries {
		if entry.IP == "" || entry.Port == "" || entry.User == "" || entry.Password == "" {
			return nil, fmt.Errorf("invalid camera entry: ip, port, user, and password are required")
		}
		baseURL := fmt.Sprintf("http://%s:%s", entry.IP, entry.Port)
		label := fmt.Sprintf("%s:%s", entry.IP, entry.Port)
		cameras = append(cameras, cameraConfig{
			BaseURL: baseURL,
			User:    entry.User,
			Pass:    entry.Password,
			Label:   label,
			Model:   entry.Model,
		})
	}
	return cameras, nil
}

func subscribeCamera(ctx context.Context, logger *log.Logger, cfg config, cam cameraConfig) (*cameraSubscription, error) {
	client, err := clientpkg.NewClient(cam.BaseURL, cam.User, cam.Pass, nil)
	if err != nil {
		return nil, fmt.Errorf("client init: %w", err)
	}
	subscribePayload, err := loadPayload(cfg, "SUBSCRIBE_PAYLOAD", "SUBSCRIBE_PAYLOAD_FILE")
	if err != nil {
		return nil, fmt.Errorf("subscribe payload: %w", err)
	}

	subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	subID, resp, err := client.Subscribe(subCtx, clientpkg.SubscribeRequest{Payload: subscribePayload})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("subscribe: %w", err)
	}
	logger.Printf("camera %s subscription created id=%s status=%d model=%s", cam.Label, subID, resp.StatusCode, cam.Model)
	return &cameraSubscription{
		camera: cam,
		client: client,
		subID:  subID,
	}, nil
}

func runCameraKeepAlive(ctx context.Context, logger *log.Logger, cfg config, sub *cameraSubscription) {
	sub.ticker = time.NewTicker(cfg.KeepAliveInterval())
	defer sub.ticker.Stop()

	for {
		select {
		case <-sub.ticker.C:
			payload, err := loadPayload(cfg, "KEEPALIVE_PAYLOAD", "KEEPALIVE_PAYLOAD_FILE")
			if err != nil {
				logger.Printf("camera %s keepalive payload error: %v", sub.camera.Label, err)
				continue
			}
			kaCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			resp, err := sub.client.KeepAlive(kaCtx, sub.subID, clientpkg.KeepAliveRequest{Payload: payload})
			cancel()
			if err != nil {
				logger.Printf("camera %s keepalive failed id=%s error=%v", sub.camera.Label, sub.subID, err)
			} else {
				logger.Printf("camera %s keepalive ok id=%s status=%d", sub.camera.Label, sub.subID, resp.StatusCode)
			}
		case <-ctx.Done():
			return
		}
	}
}
