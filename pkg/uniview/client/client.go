package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"uniview-middleware/pkg/uniview/digest"
)

const (
	defaultTimeout = 10 * time.Second
	maxRetries     = 3
)

// Client provides access to Uniview LiteAPI endpoints.
//
// NOTE: Endpoint paths are based on user-provided requirements.
// Verify request/response fields in the LiteAPI PDF before deploying.
// The payloads are passed as raw JSON to avoid inventing fields.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
}

// NewClient creates a client with digest authentication.
func NewClient(baseURL, username, password string, httpClient *http.Client) (*Client, error) {
	if baseURL == "" {
		return nil, errors.New("baseURL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse baseURL: %w", err)
	}
	transport := digest.NewTransport(username, password, nil)
	if httpClient == nil {
		httpClient = &http.Client{Transport: transport, Timeout: defaultTimeout}
	} else {
		timeout := httpClient.Timeout
		if timeout == 0 {
			timeout = defaultTimeout
		}
		httpClient = &http.Client{Transport: transport, Timeout: timeout}
	}
	return &Client{baseURL: parsed, httpClient: httpClient}, nil
}

// Subscribe creates a new event subscription.
// Payload must match the LiteAPI subscription schema.
func (c *Client) Subscribe(ctx context.Context, req SubscribeRequest) (string, *Response, error) {
	endpoint := c.endpointPath("/LAPI/V1.0/System/Event/Subscription")
	payload := bytes.NewReader(req.Payload)

	ctx, cancel := digest.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, body, err := c.doRequest(ctx, http.MethodPost, endpoint, payload, req.contentType())
	if err != nil {
		return "", nil, err
	}

	result := &Response{StatusCode: resp.StatusCode, Body: body, Headers: resp.Header.Clone()}
	if resp.StatusCode >= 300 {
		return "", result, fmt.Errorf("subscribe failed: status %d", resp.StatusCode)
	}

	id := extractSubscriptionID(body)
	return id, result, nil
}

// KeepAlive refreshes an existing subscription.
// Duration must align with the LiteAPI keepalive schema.
func (c *Client) KeepAlive(ctx context.Context, subID string, req KeepAliveRequest) (*Response, error) {
	if subID == "" {
		return nil, errors.New("subscription ID is required")
	}
	endpoint := c.endpointPath(path.Join("/LAPI/V1.0/System/Event/Subscription", subID))
	payload := bytes.NewReader(req.Payload)

	ctx, cancel := digest.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, body, err := c.doRequest(ctx, http.MethodPut, endpoint, payload, req.contentType())
	if err != nil {
		return nil, err
	}
	result := &Response{StatusCode: resp.StatusCode, Body: body, Headers: resp.Header.Clone()}
	if resp.StatusCode >= 300 {
		return result, fmt.Errorf("keepalive failed: status %d", resp.StatusCode)
	}
	return result, nil
}

// Unsubscribe cancels a subscription.
// NOTE: Verify DELETE semantics and endpoint path in the LiteAPI PDF.
func (c *Client) Unsubscribe(ctx context.Context, subID string) (*Response, error) {
	if subID == "" {
		return nil, errors.New("subscription ID is required")
	}
	endpoint := c.endpointPath(path.Join("/LAPI/V1.0/System/Event/Subscription", subID))

	ctx, cancel := digest.WithTimeout(ctx, defaultTimeout)
	defer cancel()
	resp, body, err := c.doRequest(ctx, http.MethodDelete, endpoint, nil, "")
	if err != nil {
		return nil, err
	}
	result := &Response{StatusCode: resp.StatusCode, Body: body, Headers: resp.Header.Clone()}
	if resp.StatusCode >= 300 {
		return result, fmt.Errorf("unsubscribe failed: status %d", resp.StatusCode)
	}
	return result, nil
}

// Response captures a LiteAPI response payload.
type Response struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
}

// SubscribeRequest wraps a raw JSON payload for subscription creation.
type SubscribeRequest struct {
	Payload     []byte
	ContentType string
}

func (r SubscribeRequest) contentType() string {
	if r.ContentType != "" {
		return r.ContentType
	}
	return "application/json"
}

// KeepAliveRequest wraps a raw JSON payload for keepalive.
type KeepAliveRequest struct {
	Payload     []byte
	ContentType string
}

func (r KeepAliveRequest) contentType() string {
	if r.ContentType != "" {
		return r.ContentType
	}
	return "application/json"
}

func (c *Client) endpointPath(p string) string {
	clone := *c.baseURL
	clone.Path = path.Join(strings.TrimSuffix(c.baseURL.Path, "/"), p)
	return clone.String()
}

func (c *Client) doRequest(ctx context.Context, method, endpoint string, body io.Reader, contentType string) (*http.Response, []byte, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = io.ReadAll(body)
		if err != nil {
			return nil, nil, fmt.Errorf("read payload: %w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt < maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			return nil, nil, fmt.Errorf("build request: %w", err)
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = err
			if !shouldRetry(err) || attempt == maxRetries-1 {
				return nil, nil, err
			}
			time.Sleep(backoff(attempt))
			continue
		}
		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			_ = resp.Body.Close()
			return nil, nil, fmt.Errorf("read response: %w", err)
		}
		if resp.StatusCode >= http.StatusInternalServerError && attempt < maxRetries-1 {
			_ = resp.Body.Close()
			time.Sleep(backoff(attempt))
			lastErr = fmt.Errorf("server error: %d", resp.StatusCode)
			continue
		}
		if err := resp.Body.Close(); err != nil {
			return nil, nil, fmt.Errorf("close response: %w", err)
		}
		return resp, bodyBytes, nil
	}
	return nil, nil, lastErr
}

func shouldRetry(err error) bool {
	var netErr interface{ Timeout() bool }
	if errors.As(err, &netErr) {
		return netErr.Timeout()
	}
	return true
}

func backoff(attempt int) time.Duration {
	switch attempt {
	case 0:
		return 200 * time.Millisecond
	case 1:
		return 500 * time.Millisecond
	default:
		return 1 * time.Second
	}
}

// extractSubscriptionID tries to pull a subscription identifier from a JSON response.
//
// NOTE: Field names must be confirmed in the LiteAPI PDF. Adjust candidate keys if needed.
func extractSubscriptionID(body []byte) string {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	keys := []string{"SubscriptionID", "SubscriptionId", "ID", "Id"}
	for _, key := range keys {
		if value, ok := payload[key]; ok {
			if id, ok := value.(string); ok {
				return id
			}
		}
	}
	return ""
}
