package tunnel

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/nhtera/tuzy/internal/protocol"
)

// The WebSocket dial wraps the dial error in a *url.Error carrying the visitor's URL; the page
// must show only the dial error.
func TestDialDetailDropsVisitorURL(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _, err := websocket.Dial(ctx, "http://127.0.0.1:1/secret?q=<script>", nil)
	if err == nil {
		t.Fatal("dial to port 1 succeeded")
	}
	d := dialDetail(err)
	if strings.Contains(d, "/secret") || !strings.Contains(d, "127.0.0.1:1") {
		t.Fatalf("detail %q (from %v)", d, err)
	}
}

func TestUnreachableResponseRedactsPassword(t *testing.T) {
	u, _ := url.Parse("http://user:hunter2@localhost:3000")
	for _, accept := range []string{"text/html", "*/*"} {
		_, body := unreachableResponse([]protocol.Header{{"accept", accept}}, u, nil)
		if strings.Contains(body, "hunter2") {
			t.Errorf("accept %q: password in body", accept)
		}
	}
}
