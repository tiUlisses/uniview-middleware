package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	clientpkg "uniview-middleware/pkg/uniview/client"
	configpkg "uniview-middleware/pkg/uniview/config"
	payloadpkg "uniview-middleware/pkg/uniview/payloads"
	"uniview-middleware/pkg/uniview/receiver"
	"uniview-middleware/pkg/uniview/supervisor"
)

func main() {
	if err := configpkg.LoadDotEnv(); err != nil {
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

func runServe(logger *log.Logger) {
	host := getenv("RECEIVER_LISTEN_HOST", "0.0.0.0")
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
	camera := cameraConfig{
		BaseURL: cfg.BaseURL,
		User:    cfg.User,
		Pass:    cfg.Pass,
		Label:   cfg.BaseURL,
		Model:   "uniview",
	}
	payload, err := buildSubscribePayload(cfg, camera)
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
	camera := cameraConfig{
		BaseURL: cfg.BaseURL,
		User:    cfg.User,
		Pass:    cfg.Pass,
		Label:   cfg.BaseURL,
		Model:   "uniview",
	}
	payload, err := buildKeepAlivePayload(cfg, camera, subID)
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
	host := cfg.ReceiverListenHost
	port := cfg.ReceiverPort
	cameras, err := loadCameraConfigs(cfg)
	if err != nil {
		logger.Fatalf("load cameras: %v", err)
	}
	if len(cameras) == 0 {
		logger.Fatalf("no cameras configured")
	}
	if _, err := buildSubscribePayload(cfg, cameras[0]); err != nil {
		logger.Fatalf("subscribe payload: %v", err)
	}
	if _, err := buildKeepAlivePayload(cfg, cameras[0], ""); err != nil {
		logger.Fatalf("keepalive payload: %v", err)
	}

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

	supervisorConfig := supervisor.LoadConfigFromEnv()
	payloads := payloadProvider{cfg: cfg}
	workerSupervisor := supervisor.New(logger, supervisorConfig, cfg.KeepAliveInterval(), payloads, func() ([]supervisor.CameraConfig, error) {
		return cameras, nil
	})
	if err := workerSupervisor.Run(ctx); err != nil {
		logger.Printf("supervisor stopped: %v", err)
	}

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := receiverSrv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("receiver shutdown error: %v", err)
	}
}

type cameraConfig = supervisor.CameraConfig

type payloadProvider struct {
	cfg config
}

func (p payloadProvider) SubscribePayload(camera cameraConfig) ([]byte, error) {
	return buildSubscribePayload(p.cfg, camera)
}

func (p payloadProvider) KeepAlivePayload(camera cameraConfig) ([]byte, error) {
	return buildKeepAlivePayload(p.cfg, camera, "")
}

type config struct {
	BaseURL              string
	User                 string
	Pass                 string
	ReceiverCallbackHost string
	ReceiverListenHost   string
	ReceiverPort         int
	Duration             int
	TypeMask             int
	ImagePushMode        int
}

func loadConfig() config {
	return config{
		BaseURL:              getenv("UNV_BASE_URL", ""),
		User:                 getenv("UNV_USER", ""),
		Pass:                 getenv("UNV_PASS", ""),
		ReceiverCallbackHost: getenv("RECEIVER_CALLBACK_HOST", ""),
		ReceiverListenHost:   getenv("RECEIVER_LISTEN_HOST", "0.0.0.0"),
		ReceiverPort:         getenvInt("RECEIVER_PORT", 8080),
		Duration:             getenvInt("DURATION", 60),
		TypeMask:             getenvInt("TYPE_MASK", 0),
		ImagePushMode:        getenvInt("IMAGE_PUSH_MODE", 0),
	}
}

func (c config) CallbackURL(camera cameraConfig) (string, error) {
	callbackHost, err := callbackHostForCamera(c, camera)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s:%d/LAPI/V1.0/System/Event/Notification", callbackHost, c.ReceiverPort), nil
}

func (c config) PayloadConfig(camera cameraConfig, subscriptionID string) (payloadpkg.Config, error) {
	callbackURL, err := c.CallbackURL(camera)
	if err != nil {
		return payloadpkg.Config{}, err
	}
	return payloadpkg.Config{
		CallbackURL:    callbackURL,
		Duration:       c.Duration,
		TypeMask:       c.TypeMask,
		ImagePushMode:  c.ImagePushMode,
		SubscriptionID: subscriptionID,
	}, nil
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

func buildSubscribePayload(cfg config, camera cameraConfig) ([]byte, error) {
	payloadConfig, err := cfg.PayloadConfig(camera, "")
	if err != nil {
		return nil, err
	}
	return payloadpkg.BuildSubscribePayload(payloadConfig)
}

func buildKeepAlivePayload(cfg config, camera cameraConfig, subscriptionID string) ([]byte, error) {
	payloadConfig, err := cfg.PayloadConfig(camera, subscriptionID)
	if err != nil {
		return nil, err
	}
	return payloadpkg.BuildKeepAlivePayload(payloadConfig)
}

func callbackHostForCamera(cfg config, camera cameraConfig) (string, error) {
	callbackHost := strings.TrimSpace(cfg.ReceiverCallbackHost)
	if callbackHost != "" && callbackHost != "0.0.0.0" {
		return callbackHost, nil
	}
	return localIPForCamera(camera)
}

func localIPForCamera(camera cameraConfig) (string, error) {
	host, port, err := cameraHostPort(camera.BaseURL)
	if err != nil {
		return "", err
	}
	address := net.JoinHostPort(host, port)
	conn, err := net.Dial("udp", address)
	if err != nil {
		return "", fmt.Errorf("dial camera %s: %w", address, err)
	}
	defer conn.Close()
	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || localAddr.IP == nil {
		return "", fmt.Errorf("local address unavailable for %s", address)
	}
	return localAddr.IP.String(), nil
}

func cameraHostPort(baseURL string) (string, string, error) {
	if strings.TrimSpace(baseURL) == "" {
		return "", "", fmt.Errorf("camera base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", "", fmt.Errorf("parse camera base URL: %w", err)
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("camera base URL missing host")
	}
	host := parsed.Hostname()
	if host == "" {
		return "", "", fmt.Errorf("camera base URL missing hostname")
	}
	port := parsed.Port()
	if port == "" {
		if strings.EqualFold(parsed.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, nil
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
		return nil, fmt.Errorf("forwarding requires ANALYTICS_HOST/ANALYTICS_PORT/ANALYTICS_PATH (legacy FORWARD_* supported): %w", err)
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
