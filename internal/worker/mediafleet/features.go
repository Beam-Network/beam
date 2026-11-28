package mediafleet

import (
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
)

var workerUsageFeatures = []string{mediacontracts.WorkerFeatureUsageV2, mediacontracts.WorkerFeatureMediaReceipts}

const featuresFile = "media-features"

func (f *Fleet) UsageFeatures() (usageV2, receipts bool) {
	if f == nil {
		return false, false
	}
	announced := f.announced.Load()
	if announced == nil {
		return false, false
	}
	usageV2 = slices.Contains(*announced, mediacontracts.WorkerFeatureUsageV2)
	return usageV2, usageV2 && slices.Contains(*announced, mediacontracts.WorkerFeatureMediaReceipts)
}

func (f *Fleet) usageFeatures() []string {
	var listed []string
	usageV2, receipts := f.UsageFeatures()
	if usageV2 {
		listed = append(listed, mediacontracts.WorkerFeatureUsageV2)
	}
	if receipts {
		listed = append(listed, mediacontracts.WorkerFeatureMediaReceipts)
	}
	return listed
}

func (f *Fleet) announce(features []string) {
	carried := carriedFeatures(features)
	previous := f.announced.Swap(&carried)
	if previous != nil && slices.Equal(*previous, carried) || previous == nil && len(carried) == 0 {
		return
	}
	log.Printf("media usage features announced by Core: %q", strings.Join(carried, ","))
	saveFeatures(f.config.StateDir, carried)
}

func carriedFeatures(features []string) []string {
	carried := []string{}
	for _, feature := range workerUsageFeatures {
		if slices.Contains(features, feature) {
			carried = append(carried, feature)
		}
	}
	return carried
}

func (f *Fleet) loadFeatures() {
	if strings.TrimSpace(f.config.StateDir) == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(f.config.StateDir, featuresFile))
	if err != nil {
		return
	}
	carried := carriedFeatures(strings.Split(strings.TrimSpace(string(data)), ","))
	f.announced.Store(&carried)
}

func saveFeatures(stateDir string, features []string) {
	if strings.TrimSpace(stateDir) == "" {
		return
	}
	path := filepath.Join(stateDir, featuresFile)
	if len(features) == 0 {
		_ = os.Remove(path)
		return
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return
	}
	temporary := path + ".tmp"
	if os.WriteFile(temporary, []byte(strings.Join(features, ",")), 0o600) == nil {
		_ = os.Rename(temporary, path)
	}
}
