package client

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
)

// CameraEntry represents a camera row from CSV (<ip>,<port>,<login>,<password>,<model>).
type CameraEntry struct {
	IP       string
	Port     string
	User     string
	Password string
	Model    string
}

// ParseCameraCSV reads CSV entries for camera definitions.
// Format: <ip>,<port>,<login>,<password>,<model>
// If model is empty, it defaults to "uniview".
func ParseCameraCSV(r io.Reader) ([]CameraEntry, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("read csv: %w", err)
	}
	entries := make([]CameraEntry, 0, len(records))
	for i, record := range records {
		if len(record) < 4 {
			return nil, fmt.Errorf("invalid csv line %d: expected at least 4 columns", i+1)
		}
		model := "uniview"
		if len(record) >= 5 && strings.TrimSpace(record[4]) != "" {
			model = strings.TrimSpace(record[4])
		}
		entry := CameraEntry{
			IP:       strings.TrimSpace(record[0]),
			Port:     strings.TrimSpace(record[1]),
			User:     strings.TrimSpace(record[2]),
			Password: strings.TrimSpace(record[3]),
			Model:    model,
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
