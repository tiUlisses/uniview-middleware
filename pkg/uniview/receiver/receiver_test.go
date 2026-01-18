package receiver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReceiverAck(t *testing.T) {
	received := make(chan Event, 1)
	handler := HandlerFunc(func(ctx context.Context, event Event) error {
		received <- event
		return nil
	})

	rec, err := New("127.0.0.1", 0, handler, nil)
	if err != nil {
		t.Fatalf("new receiver: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/LAPI/V1.0/System/Event/Notification/1", bytes.NewBufferString(`{"AlarmType":"Motion"}`))
	w := httptest.NewRecorder()
	rec.handleNotification(w, req)

	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", resp.StatusCode)
	}

	var ack map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	if ack["ResponseCode"].(float64) != 0 {
		t.Fatalf("unexpected response code: %v", ack["ResponseCode"])
	}
	if ack["ResponseURL"].(string) != "/LAPI/V1.0/System/Event/Notification/1" {
		t.Fatalf("unexpected response url: %v", ack["ResponseURL"])
	}

	select {
	case event := <-received:
		if event.AlarmType != "Motion" {
			t.Fatalf("unexpected alarm type: %s", event.AlarmType)
		}
	default:
		t.Fatalf("expected event to be handled")
	}
}
