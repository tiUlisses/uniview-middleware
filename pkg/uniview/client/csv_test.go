package client

import (
	"strings"
	"testing"
)

func TestParseCameraCSV(t *testing.T) {
	input := "10.0.0.1,80,admin,pass,IPC\n10.0.0.2,8080,root,secret\n"
	entries, err := ParseCameraCSV(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[1].Model != "uniview" {
		t.Fatalf("expected default model, got %s", entries[1].Model)
	}
}
