package inspector

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
)

// decodeForDisplay undoes gzip / deflate / br content-encoding for viewing only (the relayed
// bytes are never touched). Output is capped at MaxBodyCapture; truncated reports whether the
// decoded output was cut off at that cap (L3).
func decodeForDisplay(headers [][2]string, body []byte) (decoded []byte, encoding string, truncated bool, err error) {
	if len(body) == 0 {
		return nil, "", false, nil // nothing to decode (L3)
	}
	var enc string
	for _, h := range headers {
		if strings.EqualFold(h[0], "content-encoding") {
			enc = strings.ToLower(strings.TrimSpace(h[1]))
		}
	}
	var r io.Reader
	switch enc {
	case "", "identity":
		return nil, "", false, nil
	case "gzip", "x-gzip":
		zr, zerr := gzip.NewReader(bytes.NewReader(body))
		if zerr != nil {
			return nil, enc, false, zerr
		}
		r = zr
	case "deflate":
		// "deflate" is zlib-wrapped per RFC 9110, but raw deflate is common in the wild.
		if zr, zerr := zlib.NewReader(bytes.NewReader(body)); zerr == nil {
			r = zr
		} else {
			r = flate.NewReader(bytes.NewReader(body))
		}
	case "br":
		r = brotli.NewReader(bytes.NewReader(body))
	default:
		return nil, enc, false, fmt.Errorf("unsupported content-encoding %q", enc)
	}
	// Read one byte past the cap to detect truncation without guessing at the decoded size.
	out, rerr := io.ReadAll(io.LimitReader(r, MaxBodyCapture+1))
	if rerr != nil && len(out) == 0 {
		return nil, enc, false, rerr
	}
	if len(out) > MaxBodyCapture {
		out = out[:MaxBodyCapture]
		truncated = true
	}
	return out, enc, truncated, nil // a truncated capture still decodes a useful prefix
}
