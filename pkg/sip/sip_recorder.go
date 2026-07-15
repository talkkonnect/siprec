package sip

import (
	"strings"
	"sync/atomic"
	"time"

	sipparser "github.com/emiago/sipgo/sip"
	"github.com/sirupsen/logrus"

	"siprec-server/pkg/database"
)

// SIPRecorder persists captured SIP requests/responses to the database so the
// real signaling ladder of a call can be rebuilt later (e.g. by websiprec).
//
// It is deliberately best-effort and asynchronous: messages are handed to a
// buffered channel and written by a background goroutine, so database latency
// never blocks SIP transaction handling. If the buffer fills (DB stalled), new
// messages are dropped rather than backing up the call path.
type SIPRecorder struct {
	repo   *database.Repository
	logger *logrus.Logger
	ch     chan *database.SIPMessage
	done   chan struct{}
	seq    int64 // global monotonic capture counter (orders messages within a call)
}

// NewSIPRecorder starts a recorder backed by repo. Returns nil if repo is nil
// (capture disabled), so callers can store the result unconditionally.
func NewSIPRecorder(repo *database.Repository, logger *logrus.Logger) *SIPRecorder {
	if repo == nil {
		return nil
	}
	r := &SIPRecorder{
		repo:   repo,
		logger: logger,
		ch:     make(chan *database.SIPMessage, 8192),
		done:   make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *SIPRecorder) run() {
	defer close(r.done)
	for m := range r.ch {
		if err := r.repo.CreateSIPMessage(m); err != nil && r.logger != nil {
			r.logger.WithError(err).Debug("Failed to persist SIP message")
		}
	}
}

func (r *SIPRecorder) nextSeq() int {
	return int(atomic.AddInt64(&r.seq, 1))
}

// enqueue hands a message to the writer goroutine without blocking.
func (r *SIPRecorder) enqueue(m *database.SIPMessage) {
	if r == nil {
		return
	}
	select {
	case r.ch <- m:
	default:
		if r.logger != nil {
			r.logger.Warn("SIP message capture buffer full; dropping message")
		}
	}
}

// Close stops the recorder and waits for the buffered writes to flush.
func (r *SIPRecorder) Close() {
	if r == nil {
		return
	}
	close(r.ch)
	<-r.done
}

// ---- capture helpers on the server ----

// recorder returns the configured recorder (nil when capture is disabled).
func (s *CustomSIPServer) recorder() *SIPRecorder {
	if s == nil || s.handler == nil {
		return nil
	}
	return s.handler.SIPRecorder()
}

// captureInboundRequest records a received SIP request (SBC -> SRS).
func (s *CustomSIPServer) captureInboundRequest(m *SIPMessage) {
	rec := s.recorder()
	if rec == nil || m == nil || m.CallID == "" {
		return
	}
	method := m.Method
	dm := &database.SIPMessage{
		CallID:    m.CallID,
		Seq:       rec.nextSeq(),
		Timestamp: time.Now(),
		Direction: "recv",
		Method:    &method,
	}
	s.fillCommon(dm, m)
	if m.Connection != nil && m.Connection.remoteAddr != "" {
		src := m.Connection.remoteAddr
		dm.SrcAddr = &src
	}
	if len(m.RawMessage) > 0 {
		raw := string(m.RawMessage)
		dm.Raw = &raw
	}
	rec.enqueue(dm)
}

// captureOutboundResponse records a response the SRS sent (SRS -> SBC). It
// reuses the originating request's dialog headers, which are stable for the
// response, and the freshly serialized wire text of resp.
func (s *CustomSIPServer) captureOutboundResponse(message *SIPMessage, resp *sipparser.Response) {
	rec := s.recorder()
	if rec == nil || message == nil || resp == nil || message.CallID == "" {
		return
	}
	status := resp.StatusCode
	dm := &database.SIPMessage{
		CallID:     message.CallID,
		Seq:        rec.nextSeq(),
		Timestamp:  time.Now(),
		Direction:  "send",
		StatusCode: &status,
	}
	s.fillCommon(dm, message)
	if message.Connection != nil && message.Connection.remoteAddr != "" {
		dst := message.Connection.remoteAddr
		dm.DstAddr = &dst
	}
	raw := resp.String()
	dm.Raw = &raw
	rec.enqueue(dm)
}

// fillCommon populates the dialog fields shared by a request and its responses,
// sourced from the originating request wrapper.
func (s *CustomSIPServer) fillCommon(dm *database.SIPMessage, m *SIPMessage) {
	if cseq := cseqMethod(m.CSeq); cseq != "" {
		dm.CSeqMethod = &cseq
	}
	if from := firstHeader(m, "from"); from != "" {
		dm.FromURI = truncate(from, 255)
	}
	if to := firstHeader(m, "to"); to != "" {
		dm.ToURI = truncate(to, 255)
	}
	if cs := s.getCallState(m.CallID); cs != nil && cs.RecordingSession != nil && cs.RecordingSession.ID != "" {
		sid := cs.RecordingSession.ID
		dm.SessionID = &sid
	}
}

func firstHeader(m *SIPMessage, key string) string {
	if m == nil || m.Headers == nil {
		return ""
	}
	if vals := m.Headers[key]; len(vals) > 0 {
		return strings.TrimSpace(vals[0])
	}
	return ""
}

// cseqMethod extracts the method token from a CSeq value like "101 INVITE".
func cseqMethod(cseq string) string {
	fields := strings.Fields(cseq)
	if len(fields) >= 2 {
		return fields[len(fields)-1]
	}
	return ""
}

func truncate(s string, n int) *string {
	if len(s) > n {
		s = s[:n]
	}
	return &s
}
