package sfu

import (
	"fmt"
	"runtime/debug"
	"time"
)

const guardLockWait = time.Second

func (s *Session) guard(what string, closeConn func()) {
	value := recover()
	if value == nil {
		return
	}
	s.opts.Logf("sfu: session %s: recovered panic in %s: %v\n%s", s.opts.SessionID, what, value, debug.Stack())
	failure := fmt.Errorf("sfu: panic in %s: %v", what, value)
	if closeConn == nil {
		s.fail(failure)
		return
	}
	go func() {
		defer s.guard(what+" close", nil)
		if !s.lockAvailable(guardLockWait) {
			s.fail(fmt.Errorf("%w (session lock still held)", failure))
			return
		}
		closeConn()
	}()
}

func (s *Session) lockAvailable(wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if s.mu.TryLock() {
			s.mu.Unlock()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Session) fail(err error) {
	s.failOnce.Do(func() {
		s.failure = err
		close(s.failed)
	})
}

func (s *Session) Failed() <-chan struct{} { return s.failed }

func (s *Session) Failure() error {
	select {
	case <-s.failed:
		return s.failure
	default:
		return nil
	}
}
