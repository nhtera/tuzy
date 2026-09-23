package protocol

// JSON payloads of the control and head frames (§2.1). Unknown fields are ignored on decode.

// HelloMsg is the agent's first frame.
type HelloMsg struct {
	Proto      int    `json:"proto"`
	Client     string `json:"client"`
	InstanceID string `json:"instance_id"`
}

// ReadyMsg accepts the tunnel.
type ReadyMsg struct {
	Name  string `json:"name"`
	URL   string `json:"url"`
	Epoch int64  `json:"epoch"`
}

// GoawayMsg announces the edge will close the connection.
type GoawayMsg struct {
	Reason  string `json:"reason"`
	NewName string `json:"new_name,omitempty"`
	Message string `json:"message,omitempty"`
}

// GOAWAY reasons (§6).
const (
	ReasonRestart         = "restart"
	ReasonRenamed         = "renamed"
	ReasonReplaced        = "replaced"
	ReasonDeleted         = "deleted"
	ReasonSuspended       = "suspended"
	ReasonRevoked         = "revoked"
	ReasonUpgradeRequired = "upgrade_required"
)

// Header is one header line; order and duplicates are preserved.
type Header [2]string

// ReqHeadMsg opens a stream for a visitor request.
type ReqHeadMsg struct {
	Method   string   `json:"method"`
	Path     string   `json:"path"`
	Headers  []Header `json:"headers"`
	RemoteIP string   `json:"remote_ip"`
	Kind     string   `json:"kind"` // "http" | "ws"
}

// ResHeadMsg starts the response.
type ResHeadMsg struct {
	Status  int      `json:"status"`
	Headers []Header `json:"headers"`
}

// WSCloseMsg closes a visitor WebSocket.
type WSCloseMsg struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// ResetMsg aborts a stream.
type ResetMsg struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// RESET codes (§8).
const (
	ResetProtocolError = "protocol_error"
	ResetFlowControl   = "flow_control"
	ResetCreditTimeout = "credit_timeout"
	ResetHeadTimeout   = "head_timeout"
	ResetStreamTimeout = "stream_timeout"
	ResetCancelled     = "cancelled"
	ResetLocalError    = "local_error"
)
