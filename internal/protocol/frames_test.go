package protocol

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

type goldenFile struct {
	Version int `json:"version"`
	Valid   []struct {
		Name    string `json:"name"`
		Hex     string `json:"hex"`
		Decoded struct {
			Type     string `json:"type"`
			TypeCode int    `json:"type_code"`
			StreamID uint32 `json:"stream_id"`
			Payload  struct {
				Kind  string          `json:"kind"`
				Value json.RawMessage `json:"value"`
				Hex   string          `json:"hex"`
			} `json:"payload"`
		} `json:"decoded"`
	} `json:"valid"`
	Invalid []struct {
		Name  string `json:"name"`
		Hex   string `json:"hex"`
		Error string `json:"error"`
	} `json:"invalid"`
}

func loadGolden(t *testing.T) goldenFile {
	t.Helper()
	b, err := os.ReadFile("../../protocol/testdata/frames.json")
	if err != nil {
		t.Fatal(err)
	}
	var g goldenFile
	if err := json.Unmarshal(b, &g); err != nil {
		t.Fatal(err)
	}
	return g
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGoldenValid(t *testing.T) {
	for _, v := range loadGolden(t).Valid {
		t.Run(v.Name, func(t *testing.T) {
			raw := mustHex(t, v.Hex)
			f, err := Decode(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if int(f.Type) != v.Decoded.TypeCode || f.Type.String() != v.Decoded.Type {
				t.Fatalf("type = %s (%d), want %s (%d)", f.Type, f.Type, v.Decoded.Type, v.Decoded.TypeCode)
			}
			if f.StreamID != v.Decoded.StreamID {
				t.Fatalf("stream = %d, want %d", f.StreamID, v.Decoded.StreamID)
			}
			p := v.Decoded.Payload
			switch p.Kind {
			case "empty":
				if len(f.Payload) != 0 {
					t.Fatal("payload not empty")
				}
				assertHex(t, Encode(f.Type, f.StreamID, nil), v.Hex)
			case "bytes":
				if hex.EncodeToString(f.Payload) != p.Hex {
					t.Fatal("payload bytes mismatch")
				}
				assertHex(t, Encode(f.Type, f.StreamID, mustHex(t, p.Hex)), v.Hex)
			case "text":
				var s string
				_ = json.Unmarshal(p.Value, &s)
				if string(f.Payload) != s {
					t.Fatalf("text = %q, want %q", f.Payload, s)
				}
			case "u32":
				var n uint32
				_ = json.Unmarshal(p.Value, &n)
				if f.Value != n {
					t.Fatalf("value = %d, want %d", f.Value, n)
				}
				assertHex(t, EncodeU32(f.Type, f.StreamID, n), v.Hex)
			case "json":
				var got, want any
				_ = json.Unmarshal(f.Payload, &got)
				_ = json.Unmarshal(p.Value, &want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("json = %v, want %v", got, want)
				}
			default:
				t.Fatalf("unknown payload kind %q", p.Kind)
			}
		})
	}
}

func assertHex(t *testing.T, got []byte, want string) {
	t.Helper()
	if hex.EncodeToString(got) != want {
		t.Fatalf("encode mismatch")
	}
}

func TestGoldenInvalid(t *testing.T) {
	for _, v := range loadGolden(t).Invalid {
		t.Run(v.Name, func(t *testing.T) {
			_, err := Decode(mustHex(t, v.Hex))
			pe, ok := err.(*Error)
			if !ok {
				t.Fatalf("err = %v, want *Error", err)
			}
			want := map[string]ErrorKind{"malformed": KindMalformed, "stream_error": KindStreamError}[v.Error]
			if pe.Kind != want {
				t.Fatalf("kind = %v, want %s", pe.Kind, v.Error)
			}
		})
	}
}

func TestOversizedWSBinaryIsStreamError(t *testing.T) {
	_, err := Decode(Encode(WSBinary, 9, make([]byte, MaxWSMessage+1)))
	pe, ok := err.(*Error)
	if !ok || pe.Kind != KindStreamError || pe.CloseCode != 1009 || pe.StreamID != 9 {
		t.Fatalf("err = %#v", err)
	}
	if _, err := Decode(Encode(WSBinary, 9, make([]byte, MaxWSMessage))); err != nil {
		t.Fatalf("1 MiB rejected: %v", err)
	}
}

func TestEncodeJSONRoundTrip(t *testing.T) {
	b, err := EncodeJSON(Hello, 0, HelloMsg{Proto: 1, Client: "tuzy/test", InstanceID: "abcdefghijklmnop"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	var h HelloMsg
	if err := f.UnmarshalPayload(&h); err != nil || h.InstanceID != "abcdefghijklmnop" {
		t.Fatalf("roundtrip: %v %+v", err, h)
	}
}
