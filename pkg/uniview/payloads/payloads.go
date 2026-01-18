package payloads

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

const (
	addressTypeIPv4 = 0
	imagePushMin    = 0
	imagePushMax    = 2
)

// Config defines the inputs needed to build LiteAPI payloads.
type Config struct {
	CallbackURL    string
	Duration       int
	TypeMask       int
	ImagePushMode  int
	SubscriptionID string
}

// BuildSubscribePayload creates a subscription payload aligned to LiteAPI IPC V5.04.
// Fields are based on the LiteAPI Event Subscription schema.
func BuildSubscribePayload(cfg Config) ([]byte, error) {
	host, port, err := validateSubscribeConfig(cfg)
	if err != nil {
		return nil, err
	}

	payload := subscribePayload{
		AddressType:   addressTypeIPv4,
		IPAddress:     host,
		Port:          port,
		Duration:      cfg.Duration,
		Type:          cfg.TypeMask,
		ImagePushMode: cfg.ImagePushMode,
	}
	return json.Marshal(payload)
}

// BuildKeepAlivePayload creates a keepalive payload aligned to LiteAPI IPC V5.04.
func BuildKeepAlivePayload(cfg Config) ([]byte, error) {
	if cfg.Duration <= 0 {
		return nil, errors.New("duration must be greater than zero")
	}

	payload := keepAlivePayload{
		Duration: cfg.Duration,
	}
	if cfg.SubscriptionID != "" {
		payload.Reference = fmt.Sprintf("/Subscription/Subscribers/%s", cfg.SubscriptionID)
	}
	return json.Marshal(payload)
}

func validateSubscribeConfig(cfg Config) (string, int, error) {
	if cfg.CallbackURL == "" {
		return "", 0, errors.New("callback URL is required")
	}
	if cfg.Duration <= 0 {
		return "", 0, errors.New("duration must be greater than zero")
	}
	if cfg.TypeMask < 0 {
		return "", 0, errors.New("type mask must be zero or greater")
	}
	if cfg.ImagePushMode < imagePushMin || cfg.ImagePushMode > imagePushMax {
		return "", 0, fmt.Errorf("image push mode must be between %d and %d", imagePushMin, imagePushMax)
	}

	u, err := url.Parse(cfg.CallbackURL)
	if err != nil {
		return "", 0, fmt.Errorf("parse callback URL: %w", err)
	}
	if u.Host == "" {
		return "", 0, errors.New("callback URL host is required")
	}
	host := u.Hostname()
	if host == "" {
		return "", 0, errors.New("callback URL hostname is required")
	}
	portStr := u.Port()
	if portStr == "" {
		return "", 0, errors.New("callback URL port is required")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 {
		return "", 0, errors.New("callback URL port must be a positive integer")
	}
	return host, port, nil
}

type subscribePayload struct {
	AddressType   int    `json:"AddressType"`
	IPAddress     string `json:"IPAddress"`
	Port          int    `json:"Port"`
	Duration      int    `json:"Duration"`
	Type          int    `json:"Type"`
	ImagePushMode int    `json:"ImagePushMode"`
}

type keepAlivePayload struct {
	Duration  int    `json:"Duration"`
	Reference string `json:"Reference,omitempty"`
}
