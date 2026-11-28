package sfu

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Beam-Network/beam/internal/workload/contracts/mediacontracts"
	"github.com/Beam-Network/beam/internal/workload/handlers/roommedia/usage"
)

const maxReceiptBody = 2 << 10

func setReceiptConnection(w http.ResponseWriter, admitted admission) {
	if admitted.grant != nil {
		w.Header().Set(mediacontracts.ReceiptConnectionHeader, admitted.id)
		w.Header().Set("Access-Control-Expose-Headers", w.Header().Get("Access-Control-Expose-Headers")+", "+mediacontracts.ReceiptConnectionHeader)
	}
}

func (a admission) receiptKey() string {
	if a.grant == nil {
		return ""
	}
	return a.grant.Key
}

func (c *subscriberConn) receipt(command mediacontracts.ReceiptCommand) *HTTPError {
	if command.Validate() != nil {
		return commandError("receipt_invalid", "")
	}
	receipt, err := mediacontracts.ParseReceipt(command.Receipt)
	if err != nil {
		return commandError("receipt_invalid", "")
	}
	for _, connection := range c.participantConnections() {
		if connection.ID() == receipt.Connection {
			if err := connection.Receipt(command.Receipt); err != nil {
				return commandError(usage.ReceiptCode(err), "")
			}
			return nil
		}
	}
	return commandError("receipt_connection", "")
}

func (c *subscriberConn) participantConnections() []*usage.Connection {
	if c.participant == nil {
		return nil
	}
	s := c.session
	s.mu.RLock()
	defer s.mu.RUnlock()
	connections := []*usage.Connection{c.usage}
	if whep := c.participant.whep; whep != nil && whep != c {
		connections = append(connections, whep.usage)
	}
	for _, whip := range c.participant.whips {
		connections = append(connections, whip.usage)
	}
	return connections
}

func (c *subscriberConn) metered() *usage.Connection { return c.usage }
func (c *publisherConn) metered() *usage.Connection  { return c.usage }

func (s *Session) serveReceipt(w http.ResponseWriter, r *http.Request) {
	if !s.opts.Usage.TakesReceipts() {
		http.NotFound(w, r)
		return
	}
	target := s.lookup(resourceKind(r), r.PathValue("resource"))
	if target == nil {
		WriteError(w, httpError(http.StatusGone, "resource_gone", ""))
		return
	}
	if err := s.requireOwner(r, target); err != nil {
		WriteError(w, err)
		return
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		WriteError(w, httpError(http.StatusUnsupportedMediaType, "invalid_request", "Content-Type must be application/json"))
		return
	}
	var post mediacontracts.ReceiptPost
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReceiptBody+1))
	if err != nil || len(body) > maxReceiptBody || mediacontracts.UnmarshalStrict(body, &post) != nil {
		WriteError(w, httpError(http.StatusBadRequest, "invalid_request", "invalid body"))
		return
	}
	if err := target.metered().Receipt(post.Receipt); err != nil {
		WriteError(w, receiptHTTPError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func receiptHTTPError(err error) *HTTPError {
	code := usage.ReceiptCode(err)
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, usage.ErrReceiptUnbound):
		status = http.StatusNotFound
	case errors.Is(err, mediacontracts.ErrReceiptConnection):
		status = http.StatusConflict
	case errors.Is(err, mediacontracts.ErrReceiptRateLimited):
		status = http.StatusTooManyRequests
	}
	return httpError(status, code, "")
}

const receiptSweep = time.Second

func (s *Session) checkReceipts() {
	var settings *mediacontracts.UsageSettings
	var publishers []*publisherConn
	var subscribers []*subscriberConn
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if s.view != nil {
			settings = s.view.Usage
		}
		for _, conn := range s.publishers {
			if conn.usage.ReceiptBound() {
				publishers = append(publishers, conn)
			}
		}
		for _, conn := range s.subscribers {
			if conn.usage.ReceiptBound() {
				subscribers = append(subscribers, conn)
			}
		}
	}()
	for _, conn := range publishers {
		if reason := refuseReceipts(conn.usage, settings, usage.TransportWHIP); reason != "" {
			go func() {
				defer s.guard("whip receipts", nil)
				conn.receiptsRefused(reason)
			}()
		}
	}
	for _, conn := range subscribers {
		if reason := refuseReceipts(conn.usage, settings, usage.TransportWHEP); reason != "" {
			go func() {
				defer s.guard("whep receipts", nil)
				s.closedForReceipts(conn.id)
				conn.close(reason, "timeout")
			}()
		}
	}
}

func refuseReceipts(connection *usage.Connection, settings *mediacontracts.UsageSettings, transport string) string {
	reason := connection.ReceiptRefusal()
	if reason == "" {
		return ""
	}
	if !usage.EnforcesReceipts(settings, transport) {
		connection.NoteReceiptRefusal(reason)
		return ""
	}
	return reason
}

func (c *publisherConn) receiptsRefused(reason string) {
	s := c.session
	if c.closed.Load() {
		return
	}
	s.closedForReceipts(c.id)
	var events *eventChannel
	func() {
		s.mu.RLock()
		defer s.mu.RUnlock()
		if c.participant != nil && c.participant.whep != nil && c.participant.whep.usage.ReceiptBound() {
			events = c.participant.whep.ev()
		}
	}()
	if events != nil {
		id := c.usage.ID()
		events.pushValue(mediacontracts.ConnectionClosedEvent{EventHeader: header("connection_closed"), Reason: reason, ConnectionID: &id})
	}
	c.lost()
}

const receiptGoneMax = 4096

func (s *Session) closedForReceipts(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.receiptGone == nil || len(s.receiptGone) >= receiptGoneMax {
		s.receiptGone = map[string]struct{}{}
	}
	s.receiptGone[id] = struct{}{}
}

func (s *Session) goneStatus(id string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, closed := s.receiptGone[id]; closed {
		return http.StatusGone
	}
	return http.StatusNotFound
}
