package mediafleet

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const SelfTestInterval = 6 * time.Hour

const (
	selfTestDuration = 8 * time.Second
	selfTestStreams  = 4
	selfTestChunk    = 25 << 20
)

type SelfTestResult struct {
	EgressMbps  int64
	IngressMbps int64
	MeasuredAt  time.Time
}

func RunSelfTest(ctx context.Context, baseURL string) (SelfTestResult, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return SelfTestResult{}, errors.New("no self test server configured")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: false,
		MaxIdleConnsPerHost: selfTestStreams, DisableCompression: true}}
	egress, egressErr := measure(ctx, func(ctx context.Context, counter *atomic.Int64) error {
		body := &countingReader{source: newPatternReader(selfTestChunk), counter: counter}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/__up", body)
		if err != nil {
			return err
		}
		request.ContentLength = selfTestChunk
		request.Header.Set("Content-Type", "application/octet-stream")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		if response.StatusCode >= 300 {
			return fmt.Errorf("self test upload: HTTP %d", response.StatusCode)
		}
		return nil
	})
	ingress, ingressErr := measure(ctx, func(ctx context.Context, counter *atomic.Int64) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/__down?bytes=%d", baseURL, selfTestChunk), nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode >= 300 {
			return fmt.Errorf("self test download: HTTP %d", response.StatusCode)
		}
		_, err = io.Copy(io.Discard, &countingReader{source: response.Body, counter: counter})
		return err
	})
	client.CloseIdleConnections()
	result := SelfTestResult{MeasuredAt: time.Now().UTC()}
	if egressErr == nil {
		result.EgressMbps = egress
	}
	if ingressErr == nil {
		result.IngressMbps = ingress
	}
	if result.EgressMbps <= 0 && result.IngressMbps <= 0 {
		return result, errors.Join(egressErr, ingressErr)
	}
	return result, nil
}

func measure(parent context.Context, transfer func(context.Context, *atomic.Int64) error) (int64, error) {
	ctx, cancel := context.WithTimeout(parent, selfTestDuration)
	defer cancel()
	var counter atomic.Int64
	var failures atomic.Int64
	var lastErr atomic.Value
	started := time.Now()
	var group sync.WaitGroup
	for range selfTestStreams {
		group.Add(1)
		go func() {
			defer group.Done()
			for ctx.Err() == nil {
				if err := transfer(ctx, &counter); err != nil && ctx.Err() == nil {
					failures.Add(1)
					lastErr.Store(err)
					return
				}
			}
		}()
	}
	group.Wait()
	elapsed := time.Since(started).Seconds()
	moved := counter.Load()
	if moved == 0 {
		if err, ok := lastErr.Load().(error); ok {
			return 0, err
		}
		return 0, errors.New("self test moved no data")
	}
	return int64(float64(moved) * 8 / elapsed / 1_000_000), nil
}

type countingReader struct {
	source  io.Reader
	counter *atomic.Int64
}

func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.source.Read(buffer)
	r.counter.Add(int64(read))
	return read, err
}

type patternReader struct {
	block     []byte
	remaining int64
	offset    int
}

func newPatternReader(size int64) *patternReader {
	block := make([]byte, 256<<10)
	_, _ = rand.Read(block)
	return &patternReader{block: block, remaining: size}
}

func (r *patternReader) Read(buffer []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	written := 0
	for written < len(buffer) && r.remaining > 0 {
		chunk := copy(buffer[written:], r.block[r.offset:])
		chunk = int(min(int64(chunk), r.remaining))
		written += chunk
		r.remaining -= int64(chunk)
		r.offset = (r.offset + chunk) % len(r.block)
	}
	return written, nil
}

type hostSampler struct {
	mu        sync.Mutex
	lastAt    time.Time
	lastBusy  uint64
	lastTotal uint64
	lastTX    uint64
	lastRX    uint64
	cpuRatio  float64
	egress    int64
	ingress   int64
}

func (s *hostSampler) sample(now time.Time) {
	busy, total, cpuOK := readProcStat()
	tx, rx, netOK := readProcNetDev()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastAt.IsZero() {
		if cpuOK && total > s.lastTotal {
			s.cpuRatio = min(1, max(0, float64(busy-s.lastBusy)/float64(total-s.lastTotal)))
		}
		if seconds := now.Sub(s.lastAt).Seconds(); netOK && seconds > 0 && tx >= s.lastTX && rx >= s.lastRX {
			s.egress = int64(float64(tx-s.lastTX) * 8 / seconds / 1_000_000)
			s.ingress = int64(float64(rx-s.lastRX) * 8 / seconds / 1_000_000)
		}
	}
	s.lastAt, s.lastBusy, s.lastTotal, s.lastTX, s.lastRX = now, busy, total, tx, rx
}

func (s *hostSampler) values() (float64, int64, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cpuRatio, s.egress, s.ingress
}

func readProcStat() (busy, total uint64, ok bool) {
	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	var idle uint64
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += value
		if index == 3 || index == 4 {
			idle += value
		}
	}
	return total - idle, total, true
}

func readProcNetDev() (tx, rx uint64, ok bool) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		name, counters, found := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !found || name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "br-") {
			continue
		}
		fields := strings.Fields(counters)
		if len(fields) < 9 {
			continue
		}
		received, errRX := strconv.ParseUint(fields[0], 10, 64)
		sent, errTX := strconv.ParseUint(fields[8], 10, 64)
		if errRX != nil || errTX != nil {
			continue
		}
		rx += received
		tx += sent
		ok = true
	}
	return tx, rx, ok
}

const maxCacheWalkEntries = 2_000_000

func cacheUsedBytes(dir string) int64 {
	if dir == "" {
		return 0
	}
	var total int64
	entries := 0
	_ = filepath.WalkDir(dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		entries++
		if entries > maxCacheWalkEntries {
			return filepath.SkipAll
		}
		if entry.Type().IsRegular() {
			if info, err := entry.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
