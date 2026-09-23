package tunnel

import "github.com/nhtera/tuzy/internal/protocol"

// Recorder observes relayed traffic (the local inspector). Implementations must be quick and
// never block: they are called on the proxy path.
type Recorder interface {
	Begin(tunnel string, head protocol.ReqHeadMsg) StreamRecorder
}

// StreamRecorder observes one stream.
type StreamRecorder interface {
	RequestBody(p []byte)
	// RequestEnd is called when the edge's REQ_END is seen (the visitor's request body ended
	// normally on the wire). It is never called if the stream is reset or the visitor aborts
	// first, so a recorder can use its absence to flag an incomplete request body.
	RequestEnd()
	Response(status int, headers []protocol.Header)
	ResponseBody(p []byte)
	WSMessage(fromVisitor bool)
	End(errMsg string)
}

type nopRecorder struct{}

func (nopRecorder) RequestBody([]byte)              {}
func (nopRecorder) RequestEnd()                     {}
func (nopRecorder) Response(int, []protocol.Header) {}
func (nopRecorder) ResponseBody([]byte)             {}
func (nopRecorder) WSMessage(bool)                  {}
func (nopRecorder) End(string)                      {}

// recorderWriter adapts StreamRecorder.ResponseBody to io.Writer (for io.TeeReader).
type recorderWriter struct{ r StreamRecorder }

func (w recorderWriter) Write(p []byte) (int, error) {
	w.r.ResponseBody(p)
	return len(p), nil
}

func (s *session) begin(head protocol.ReqHeadMsg) StreamRecorder {
	if s.cfg.recorder == nil {
		return nopRecorder{}
	}
	return s.cfg.recorder.Begin(s.name, head)
}
