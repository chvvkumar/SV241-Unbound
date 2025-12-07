package telemetry

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"sv241pro-alpaca-proxy/internal/logger"
)

type DataPoint struct {
	Timestamp int64   `json:"t"`
	Voltage   float64 `json:"v"`
	Current   float64 `json:"c"`
	Power     float64 `json:"p"`
	TempAmb   float64 `json:"temp"`
	HumAmb    float64 `json:"hum"`
	DewPoint  float64 `json:"dew"`
	TempLens  float64 `json:"lens"`
	PWM1      int     `json:"pwm1"`
	PWM2      int     `json:"pwm2"`
}

// HandleGetHistory reads from the CSV logs and returns JSON data.
// By default, it returns data from the current "night" and the previous "night".
func HandleGetHistory(w http.ResponseWriter, r *http.Request) {
	// Simple strategy: Locate the last 2 log files.
	logFiles, err := getRecentLogFiles(2)
	if err != nil {
		http.Error(w, "Failed to list log files", http.StatusInternalServerError)
		return
	}

	var history []DataPoint

	for _, filename := range logFiles {
		path := filepath.Join(GetLogsDir(), filename)
		file, err := os.Open(path)
		if err != nil {
			logger.Warn("Failed to open log file %s: %v", filename, err)
			continue
		}
		defer file.Close()

		reader := csv.NewReader(file)
		// Skip header if present (heuristic: first field is "timestamp")
		firstLine, err := reader.Read()
		if err != nil {
			continue
		}
		if firstLine[0] != "timestamp" {
			// If it's not a header, parse it
			if dp, err := parseRecord(firstLine); err == nil {
				history = append(history, dp)
			}
		}

		for {
			record, err := reader.Read()
			if err == io.EOF {
				break
			}
			if err != nil {
				continue
			}
			if dp, err := parseRecord(record); err == nil {
				history = append(history, dp)
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(history)
}

func getRecentLogFiles(count int) ([]string, error) {
	dir := GetLogsDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "telemetry_") {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)

	if len(files) > count {
		return files[len(files)-count:], nil
	}
	return files, nil
}

func parseRecord(record []string) (DataPoint, error) {
	// Format: timestamp_iso, voltage, current, power, temp_amb, hum_amb, dew_point, temp_lens, pwm1, pwm2
	if len(record) < 10 {
		return DataPoint{}, fmt.Errorf("invalid record length")
	}

	ts, err := time.Parse(time.RFC3339, record[0])
	if err != nil {
		return DataPoint{}, err
	}

	// Helper helper
	pf := func(s string) float64 {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	pi := func(s string) int {
		v, _ := strconv.Atoi(s)
		return v
	}

	return DataPoint{
		Timestamp: ts.Unix(),
		Voltage:   pf(record[1]),
		Current:   pf(record[2]),
		Power:     pf(record[3]),
		TempAmb:   pf(record[4]),
		HumAmb:    pf(record[5]),
		DewPoint:  pf(record[6]),
		TempLens:  pf(record[7]),
		PWM1:      pi(record[8]),
		PWM2:      pi(record[9]),
	}, nil
}
