package telemetry

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sv241pro-alpaca-proxy/internal/logger"
)

// PruneOldFiles ensures that we satisfy the retention policy.
// It keeps the 'maxNights' most recent files and deletes the rest.
func PruneOldFiles(dir string, maxNights int) {
	if maxNights <= 0 {
		return // Infinite retention or disabled
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		logger.Error("PruneOldFiles: Failed to read directory: %v", err)
		return
	}

	var telemetryFiles []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), "telemetry_") && strings.HasSuffix(entry.Name(), ".csv") {
			telemetryFiles = append(telemetryFiles, entry.Name())
		}
	}

	// Sort files alphabetically.
	// Since filenames contained YYYY-MM-DD, alphabetical order == chronological order.
	sort.Strings(telemetryFiles)

	if len(telemetryFiles) <= maxNights {
		return // Within limits
	}

	// Calculate how many to delete
	deleteCount := len(telemetryFiles) - maxNights
	filesToDelete := telemetryFiles[:deleteCount]

	for _, filename := range filesToDelete {
		fullPath := filepath.Join(dir, filename)
		if err := os.Remove(fullPath); err != nil {
			logger.Warn("Failed to prune old log file %s: %v", filename, err)
		} else {
			logger.Info("Pruned old log file: %s", filename)
		}
	}
}
