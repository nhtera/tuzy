// Package protocol implements the tuzy wire protocol v1 frame codec (protocol/PROTOCOL.md §2).
//
//	frame := type:u8 | stream_id:u32 (big-endian) | payload:bytes
//
// Decode enforces every connection-level "malformed" rule of §2.2 (the receiver closes the agent
// socket with 1002) and reports per-stream WebSocket errors as KindStreamError.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

// FrameType is the first byte of every frame.
type FrameType uint8

// Frame types (§2.1).
const (
	Hello    FrameType = 0x01
	Ready    FrameType = 0x02
	Goaway   FrameType = 0x03
	Drain    FrameType = 0x04
	ReqHead  FrameType = 0x10
	ReqBody  FrameType = 0x11
	ReqEnd   FrameType = 0x12
	ResHead  FrameType = 0x20
	ResBody  FrameType = 0x21
	ResEnd   FrameType = 0x22
	WSText   FrameType = 0x30
	WSBinary FrameType = 0x31
	WSClose  FrameType = 0x32
	Window   FrameType = 0x40
	Reset    FrameType = 0x41
	Ack      FrameType = 0x42
)

var typeNames = map[FrameType]string{
	Hello: "HELLO", Ready: "READY", Goaway: "GOAWAY", Drain: "DRAIN",
	ReqHead: "REQ_HEAD", ReqBody: "REQ_BODY", ReqEnd: "REQ_END",
	ResHead: "RES_HEAD", ResBody: "RES_BODY", ResEnd: "RES_END",
	WSText: "WS_TEXT", WSBinary: "WS_BINARY", WSClose: "WS_CLOSE",
	Window: "WINDOW", Reset: "RESET", Ack: "ACK",
}

func (t FrameType) String() string {
	if n, ok := typeNames[t]; ok {
		return n
	}
	return fmt.Sprintf("0x%02x", uint8(t))
}

// Normative wire constants (§2.2). Edge policy timeouts are not part of the protocol.
const (
	HeaderSize     = 5
	MaxBodyChunk   = 64 * 1024
	MaxWSMessage   = 1024 * 1024
	MaxJSONPayload = 128 * 1024
	StreamWindow   = 1024 * 1024
	ConnWindow     = 8 * 1024 * 1024
	MaxWindow      = 1<<31 - 1
	WSMsgCredit    = 8
	WSMsgTolerance = 16
	MaxStreams     = 128
)

// Heartbeat text messages (§7).
const (
	Ping = "tuzy-ping"
	Pong = "tuzy-pong"
)

// ErrorKind classifies decode failures.
type ErrorKind int

const (
	// KindMalformed is a connection error: close the agent socket with 1002.
	KindMalformed ErrorKind = iota
	// KindStreamError affects one visitor WebSocket only (1007 / 1009).
	KindStreamError
)

// Error is a protocol violation found while decoding.
type Error struct {
	Kind      ErrorKind
	StreamID  uint32
	CloseCode int // for KindStreamError: 1007 (invalid UTF-8) or 1009 (too big)
	Msg       string
}

func (e *Error) Error() string { return "protocol: " + e.Msg }

func malformed(format string, a ...any) error {
	return &Error{Kind: KindMalformed, Msg: fmt.Sprintf(format, a...)}
}

// IsMalformed reports whether err is a connection-level protocol error.
func IsMalformed(err error) bool {
	var pe *Error
	return errors.As(err, &pe) && pe.Kind == KindMalformed
}

// Frame is a decoded frame. Payload aliases the input buffer.
type Frame struct {
	Type     FrameType
	StreamID uint32
	Payload  []byte
	// Value is the credit for WINDOW / ACK.
	Value uint32
}

func (t FrameType) known() bool { _, ok := typeNames[t]; return ok }

func (t FrameType) stream0Only() bool { return t == Hello || t == Ready || t == Goaway || t == Drain }

// IsJSON reports whether the payload of t is a JSON object.
func (t FrameType) IsJSON() bool {
	switch t {
	case Hello, Ready, Goaway, ReqHead, ResHead, WSClose, Reset:
		return true
	}
	return false
}

func (t FrameType) mustBeEmpty() bool { return t == Drain || t == ReqEnd || t == ResEnd }

func (t FrameType) isU32() bool { return t == Window || t == Ack }

// Decode parses one binary WebSocket message, validating §2.2.
func Decode(msg []byte) (Frame, error) {
	if len(msg) < HeaderSize {
		return Frame{}, malformed("frame shorter than header")
	}
	t := FrameType(msg[0])
	if !t.known() {
		return Frame{}, malformed("unknown frame type 0x%02x", msg[0])
	}
	f := Frame{Type: t, StreamID: binary.BigEndian.Uint32(msg[1:5]), Payload: msg[HeaderSize:]}

	if t.stream0Only() {
		if f.StreamID != 0 {
			return Frame{}, malformed("%s on non-zero stream", t)
		}
	} else if t != Window && f.StreamID == 0 {
		return Frame{}, malformed("%s on stream 0", t)
	}
	if t.mustBeEmpty() && len(f.Payload) != 0 {
		return Frame{}, malformed("%s payload must be empty", t)
	}
	if t.isU32() {
		if len(f.Payload) != 4 {
			return Frame{}, malformed("%s payload must be 4 bytes", t)
		}
		f.Value = binary.BigEndian.Uint32(f.Payload)
		if f.Value == 0 {
			return Frame{}, malformed("%s value must be > 0", t)
		}
	}
	if t.IsJSON() {
		if len(f.Payload) > MaxJSONPayload {
			return Frame{}, malformed("JSON payload too large")
		}
		if !isJSONObject(f.Payload) {
			return Frame{}, malformed("%s payload must be a JSON object", t)
		}
	}
	if (t == ReqBody || t == ResBody) && len(f.Payload) > MaxBodyChunk {
		return Frame{}, malformed("body chunk larger than 64 KiB")
	}
	if t == WSText || t == WSBinary {
		if len(f.Payload) > MaxWSMessage {
			return Frame{}, &Error{Kind: KindStreamError, StreamID: f.StreamID, CloseCode: 1009, Msg: "ws message too big"}
		}
		if t == WSText && !utf8.Valid(f.Payload) {
			return Frame{}, &Error{Kind: KindStreamError, StreamID: f.StreamID, CloseCode: 1007, Msg: "ws text is not valid UTF-8"}
		}
	}
	return f, nil
}

// isJSONObject reports whether b is exactly one valid JSON object (no trailing data).
func isJSONObject(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return false
	}
	return obj != nil // `null` unmarshals into a nil map
}

// Encode builds a frame with a raw payload.
func Encode(t FrameType, streamID uint32, payload []byte) []byte {
	out := make([]byte, HeaderSize+len(payload))
	out[0] = byte(t)
	binary.BigEndian.PutUint32(out[1:5], streamID)
	copy(out[HeaderSize:], payload)
	return out
}

// EncodeJSON builds a JSON frame from v (a struct or map).
func EncodeJSON(t FrameType, streamID uint32, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return Encode(t, streamID, b), nil
}

// EncodeU32 builds a WINDOW or ACK frame.
func EncodeU32(t FrameType, streamID, value uint32) []byte {
	var p [4]byte
	binary.BigEndian.PutUint32(p[:], value)
	return Encode(t, streamID, p[:])
}

// UnmarshalPayload decodes a JSON frame payload into v.
func (f Frame) UnmarshalPayload(v any) error {
	if err := json.Unmarshal(f.Payload, v); err != nil {
		return &Error{Kind: KindMalformed, Msg: fmt.Sprintf("bad %s payload: %v", f.Type, err)}
	}
	return nil
}
