package gpulock

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
)

const DefaultDirectory = "/run/beam-worker/gpu-locks"

func Owner(directory string) string {
	sum := sha256.Sum256([]byte(directory))
	return hex.EncodeToString(sum[:])
}

type BusyError struct {
	Free, Requested int64
}

func (e *BusyError) Error() string {
	return fmt.Sprintf("only %d of the %d requested GPUs are free", e.Free, e.Requested)
}

type HeldError struct {
	Device int
}

func (e *HeldError) Error() string {
	return fmt.Sprintf("GPU %d is held by a GPU seat", e.Device)
}

type Lock struct {
	file *os.File
}

func (l *Lock) File() *os.File {
	if l == nil {
		return nil
	}
	return l.file
}

func (l *Lock) Release() {
	if l != nil && l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

const MaxDevices = 64

func ParseDevices(value string) ([]int, error) {
	var devices []int
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		device, err := strconv.Atoi(entry)
		if err != nil || device < 0 || device >= MaxDevices {
			return nil, fmt.Errorf("GPU %q is not a device index 0..%d", entry, MaxDevices-1)
		}
		if slices.Contains(devices, device) {
			return nil, fmt.Errorf("GPU %d is listed twice", device)
		}
		devices = append(devices, device)
	}
	slices.Sort(devices)
	return devices, nil
}

func FormatDevices(devices []int) string {
	parts := make([]string, len(devices))
	for i, device := range devices {
		parts[i] = strconv.Itoa(device)
	}
	return strings.Join(parts, ",")
}

func Range(count int64) []int {
	devices := make([]int, 0, max(count, 0))
	for device := int64(0); device < count; device++ {
		devices = append(devices, int(device))
	}
	return devices
}

func Overlap(a, b []int) []int {
	var both []int
	for _, device := range a {
		if slices.Contains(b, device) && !slices.Contains(both, device) {
			both = append(both, device)
		}
	}
	slices.Sort(both)
	return both
}
