package sfu

import (
	"bytes"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/pion/webrtc/v4"
)

const (
	eventQueueLimit   = 4096
	maxCommandBytes   = 128 << 10
	eventBufferHigh   = 1 << 20
	eventBufferLow    = 256 << 10
	eventDrainTimeout = 10 * time.Second
	maxErrorDetail    = 1024
)

type event struct {
	template []byte
}

var headerPlaceholder = []byte(`,"seq":0,"ts_ms":0`)

func newEvent(value any) (event, error) {
	encoded, err := mediacontracts.MarshalWire(value)
	if err != nil {
		return event{}, err
	}
	if !bytes.Contains(encoded, headerPlaceholder) || len(encoded) > mediacontracts.EventMessageMaxBytes {
		return event{}, errEventShape
	}
	return event{template: encoded}, nil
}

type eventError string

func (e eventError) Error() string { return string(e) }

const errEventShape = eventError("sfu: event does not fit beam.events.v1")

func header(eventType string) mediacontracts.EventHeader {
	return mediacontracts.EventHeader{Type: eventType}
}

type eventChannel struct {
	dc *webrtc.DataChannel

	mu       sync.Mutex
	seq      int64
	queue    [][]byte
	ready    bool
	closed   bool
	signal   chan struct{}
	low      chan struct{}
	overflow func()
	logf     func(format string, args ...any)
}

func newEventChannel(dc *webrtc.DataChannel, overflow func(), logf func(string, ...any)) *eventChannel {
	channel := &eventChannel{dc: dc, signal: make(chan struct{}, 1), low: make(chan struct{}, 1), overflow: overflow, logf: logf}
	dc.SetBufferedAmountLowThreshold(eventBufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case channel.low <- struct{}{}:
		default:
		}
	})
	go channel.loop()
	return channel
}

func (c *eventChannel) push(value event) {
	if c == nil {
		return
	}
	queued, overflowed := c.enqueue(value)
	if overflowed && c.overflow != nil {
		go c.overflow()
	}
	if !queued {
		return
	}
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *eventChannel) enqueue(value event) (queued, overflowed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false, false
	}
	if len(c.queue) >= eventQueueLimit {
		c.closed = true
		return false, true
	}
	c.seq++
	stamped := bytes.Replace(value.template, headerPlaceholder,
		[]byte(`,"seq":`+strconv.FormatInt(c.seq, 10)+`,"ts_ms":`+strconv.FormatInt(time.Now().UnixMilli(), 10)), 1)
	c.queue = append(c.queue, stamped)
	return true, false
}

func (c *eventChannel) next() (message []byte, ok, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.queue) == 0 {
		return nil, false, c.closed
	}
	message = c.queue[0]
	c.queue = c.queue[1:]
	return message, true, false
}

func (c *eventChannel) shut() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed, c.queue = true, nil
}

func (c *eventChannel) queueEmpty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.queue) == 0
}

func (c *eventChannel) pushValue(value any) {
	encoded, err := newEvent(value)
	if err == nil {
		c.push(encoded)
	}
}

func (c *eventChannel) isReady() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ready && !c.closed
}

func (c *eventChannel) setReady() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ready = true
}

func (c *eventChannel) close() {
	if c == nil {
		return
	}
	func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.closed = true
	}()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *eventChannel) loop() {
	defer func() {
		if value := recover(); value != nil {
			if c.logf != nil {
				c.logf("sfu: recovered panic in the event channel: %v\n%s", value, debug.Stack())
			}
			c.shut()
			if c.overflow != nil {
				go c.overflow()
			}
		}
	}()
	for range c.signal {
		for {
			next, ok, closed := c.next()
			if !ok {
				if closed {
					return
				}
				break
			}
			if !c.drained() {
				c.shut()
				if c.overflow != nil {
					go c.overflow()
				}
				return
			}
			if err := c.dc.SendText(string(next)); err != nil {
				c.shut()
				return
			}
		}
	}
}

func (c *eventChannel) drained() bool {
	if c.dc.BufferedAmount() <= eventBufferHigh {
		return true
	}
	deadline := time.NewTimer(eventDrainTimeout)
	defer deadline.Stop()
	for c.dc.BufferedAmount() > eventBufferHigh {
		select {
		case <-c.low:
		case <-deadline.C:
			return false
		case <-time.After(250 * time.Millisecond):
		}
	}
	return true
}

func (c *eventChannel) waitSent(wait time.Duration) {
	if c == nil {
		return
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if c.queueEmpty() && c.dc.BufferedAmount() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (c *eventChannel) flushAndClose(wait time.Duration) {
	if c == nil {
		return
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if c.queueEmpty() && c.dc.BufferedAmount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.close()
}

func errorEvent(id *string, code, detail string) mediacontracts.ErrorEvent {
	value := mediacontracts.ErrorEvent{EventHeader: header("error"), ID: id, Code: code}
	if len(detail) > maxErrorDetail {
		detail = strings.ToValidUTF8(detail[:maxErrorDetail], "")
	}
	if detail != "" {
		value.Detail = &detail
	}
	return value
}
