package im_test

import (
	"fmt"
	"net/http/httptrace"
	"sync"
	"time"
)

// Only transport state is captured: no URL parameters, headers or request bodies.
type registrationTrace struct {
	mu                                                               sync.Mutex
	gotConn, reused, wasIdle, wroteRequest, firstByte, connectFailed bool
	idle                                                             time.Duration
}

func (s *registrationTrace) callbacks() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.gotConn, s.reused, s.wasIdle, s.idle = true, info.Reused, info.WasIdle, info.IdleTime
		},
		ConnectDone: func(_, _ string, err error) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.connectFailed = s.connectFailed || err != nil
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.wroteRequest = info.Err == nil
		},
		GotFirstResponseByte: func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.firstByte = true
		},
	}
}

func (s *registrationTrace) wrap(err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fmt.Errorf("registration transport got_conn=%t reused=%t was_idle=%t idle_ms=%d wrote_request=%t first_byte=%t connect_failed=%t: %w",
		s.gotConn, s.reused, s.wasIdle, s.idle.Milliseconds(), s.wroteRequest, s.firstByte, s.connectFailed, err)
}
