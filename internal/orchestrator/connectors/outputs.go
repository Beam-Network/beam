package connectors

import "strings"

// resultOutput returns a chunk-level output such as "sha256" or "etag".
//
// The transfer handler keys its outputs per part ("part.<index>.sha256",
// "part.<index>.etag"), while a BeamCore task_result carries exactly one
// chunk. Prefer a bare key; otherwise use the value of the sole part.
// Return "" when the value is missing or there is more than one part, so
// callers never report a hash that belongs to a different chunk.
func resultOutput(outputs map[string]string, name string) string {
	if value := outputs[name]; value != "" {
		return value
	}
	suffix := "." + name
	found, count := "", 0
	for key, value := range outputs {
		if value != "" && strings.HasPrefix(key, "part.") && strings.HasSuffix(key, suffix) {
			found, count = value, count+1
		}
	}
	if count == 1 {
		return found
	}
	return ""
}
