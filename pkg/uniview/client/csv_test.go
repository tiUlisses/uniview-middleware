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

func TestParseCameraCSVSkipsEmptyCommentsAndHeader(t *testing.T) {
	input := "# comment line\n\nip,port,login,password,model\n10.0.0.1,80,admin,pass,IPC\n#another comment\n10.0.0.2,8080,root,secret\n"
	entries, err := ParseCameraCSV(strings.NewReader(input))
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].Model != "IPC" {
		t.Fatalf("expected model IPC, got %s", entries[0].Model)
	}
	if entries[1].Model != "uniview" {
		t.Fatalf("expected default model, got %s", entries[1].Model)
	}
}

func TestParseCameraCSVInvalidLineStillErrors(t *testing.T) {
	input := "ip,port,login,password,model\n10.0.0.1,80,admin\n"
	_, err := ParseCameraCSV(strings.NewReader(input))
	if err == nil {
		t.Fatal("expected error for invalid data line")
	}
}
