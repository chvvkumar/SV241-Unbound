package telemetry

import (
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"sv241pro-alpaca-proxy/internal/config"
	"sv241pro-alpaca-proxy/internal/logger"
	"sv241pro-alpaca-proxy/internal/serial"
)

var (
	loggerC      *CSVLogger
	logsDir      string
	logFileMutex sync.Mutex
)

// CSVLogger handles writing telemetry to disk.
type CSVLogger struct {
	currentFile *os.File
	csvWriter   *csv.Writer
	nightDate   string // YYYY-MM-DD string representing the "Observing Night"
}

// Init initializes the logging system.
func Init() {
	configDir, _ := os.UserConfigDir()
	logsDir = filepath.Join(configDir, "SV241AlpacaProxy", "logs")
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		logger.Error("Failed to create logs directory: %v", err)
		return
	}
	// Start the logging loop
	go loggingLoop()
}

func loggingLoop() {
	conf := config.Get()
	interval := time.Duration(conf.TelemetryInterval) * time.Second
	if interval == 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	logger.Info("CSV Telemetry Logging started. Interval: %v", interval)

	for range ticker.C {
		logTelemetry()
	}
}

func logTelemetry() {
	// 1. Get current conditions (thread-safe copy)
	serial.Conditions.RLock()
	data := serial.Conditions.Data
	serial.Conditions.RUnlock()

	if data == nil {
		return // No data yet
	}

	// 2. Determine "Night Date" (Noon to Noon)
	now := time.Now()
	nightDate := getNightDate(now)

	// 3. Write to file
	logFileMutex.Lock()
	defer logFileMutex.Unlock()

	if loggerC == nil || loggerC.nightDate != nightDate {
		rotateLogFile(nightDate)
		// Prune old files after rotation
		PruneOldFiles(logsDir, config.Get().HistoryRetentionNights)
	}

	if loggerC != nil && loggerC.csvWriter != nil {
		// Prepare CSV record
		// Format: timestamp_iso, voltage, current, power, temp_amb, hum_amb, dew_point, temp_lens, pwm1, pwm2
		record := []string{
			now.Format(time.RFC3339),
			fmt.Sprintf("%v", data["v"]),
			fmt.Sprintf("%v", data["i"]),
			fmt.Sprintf("%v", data["p"]),
			fmt.Sprintf("%v", data["t_amb"]),
			fmt.Sprintf("%v", data["h_amb"]),
			fmt.Sprintf("%v", data["d"]),
			fmt.Sprintf("%v", data["t_lens"]),
			fmt.Sprintf("%v", data["pwm1"]),
			fmt.Sprintf("%v", data["pwm2"]),
		}
		if err := loggerC.csvWriter.Write(record); err != nil {
			logger.Error("Failed to write to CSV: %v", err)
		} else {
			loggerC.csvWriter.Flush() // Ensure data hits disk (or OS buffer)
		}
	}
}

// getNightDate returns the date string for the "observing night".
// If time is before 12:00 PM (noon), it belongs to the previous day.
func getNightDate(t time.Time) string {
	if t.Hour() < 12 {
		return t.AddDate(0, 0, -1).Format("2006-01-02")
	}
	return t.Format("2006-01-02")
}

func rotateLogFile(newDate string) {
	if loggerC != nil && loggerC.currentFile != nil {
		loggerC.csvWriter.Flush()
		loggerC.currentFile.Close()
	}

	filename := filepath.Join(logsDir, fmt.Sprintf("telemetry_%s.csv", newDate))

	// Check if file exists to decide whether to write header
	writeHeader := false
	if _, err := os.Stat(filename); os.IsNotExist(err) {
		writeHeader = true
	}

	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		logger.Error("Failed to open log file %s: %v", filename, err)
		return
	}

	loggerC = &CSVLogger{
		currentFile: f,
		csvWriter:   csv.NewWriter(f),
		nightDate:   newDate,
	}

	if writeHeader {
		header := []string{"timestamp", "voltage", "current", "power", "t_amb", "h_amb", "dew_point", "t_lens", "pwm1", "pwm2"}
		loggerC.csvWriter.Write(header)
		loggerC.csvWriter.Flush()
	}
	logger.Info("Rotated telemetry log to: %s", filename)
}

// GetLogsDir returns the path to the logs directory
func GetLogsDir() string {
	// Re-construct if not initialized (though Init should be called)
	if logsDir == "" {
		configDir, _ := os.UserConfigDir()
		return filepath.Join(configDir, "SV241AlpacaProxy", "logs")
	}
	return logsDir
}
