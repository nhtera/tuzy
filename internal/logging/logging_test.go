package logging

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nhtera/tuzy/internal/tunnel"
)

var ts = regexp.MustCompile(`(?m)^\d\d:\d\d:\d\d |"time":"[^"]+",|time=\S+ `)

func emit(t *testing.T, format, level string) string {
	t.Helper()
	var out bytes.Buffer
	l, err := Open(Options{Dest: "stdout", Format: format, Level: level}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Event(tunnel.Event{Kind: tunnel.EventOnline, Name: "shop", URL: "https://shop.tuzy.dev", Epoch: 3}, "http://localhost:3000")
	l.Event(tunnel.Event{Kind: tunnel.EventReconnecting, Name: "shop", RetryIn: 1500 * time.Millisecond, Err: errors.New("eof")}, "")
	l.Access("shop", tunnel.AccessEntry{Kind: "http", Method: "POST", Path: "/hook?a=1 b", Status: 502, Bytes: 12, Duration: 42 * time.Millisecond, RemoteIP: "203.0.113.9", Err: "local_error"})
	l.Access("shop", tunnel.AccessEntry{Kind: "http", Method: "GET", Path: "/\x1b[31mred", Status: 200})
	return ts.ReplaceAllString(out.String(), "")
}

func TestFormatsGolden(t *testing.T) {
	cases := map[string]string{
		"term": `INF tunnel.online name=shop url=https://shop.tuzy.dev target=http://localhost:3000 epoch=3
WRN tunnel.reconnecting name=shop retry_in=1.5s error=eof
WRN request tunnel=shop method=POST path="/hook?a=1 b" status=502 duration_ms=42 bytes=12 remote_ip=203.0.113.9 error=local_error
INF request tunnel=shop method=GET path="/\x1b[31mred" status=200 duration_ms=0 bytes=0 remote_ip=""
`,
		"logfmt": `level=INFO msg=tunnel.online name=shop url=https://shop.tuzy.dev target=http://localhost:3000 epoch=3
level=WARN msg=tunnel.reconnecting name=shop retry_in=1.5s error=eof
level=WARN msg=request tunnel=shop method=POST path="/hook?a=1 b" status=502 duration_ms=42 bytes=12 remote_ip=203.0.113.9 error=local_error
level=INFO msg=request tunnel=shop method=GET path="/\x1b[31mred" status=200 duration_ms=0 bytes=0 remote_ip=""
`,
		"json": `{"level":"INFO","msg":"tunnel.online","name":"shop","url":"https://shop.tuzy.dev","target":"http://localhost:3000","epoch":3}
{"level":"WARN","msg":"tunnel.reconnecting","name":"shop","retry_in":"1.5s","error":"eof"}
{"level":"WARN","msg":"request","tunnel":"shop","method":"POST","path":"/hook?a=1 b","status":502,"duration_ms":42,"bytes":12,"remote_ip":"203.0.113.9","error":"local_error"}
{"level":"INFO","msg":"request","tunnel":"shop","method":"GET","path":"/\u001b[31mred","status":200,"duration_ms":0,"bytes":0,"remote_ip":""}
`,
	}
	for format, want := range cases {
		if got := emit(t, format, ""); got != want {
			t.Errorf("%s:\n%s\nwant:\n%s", format, got, want)
		}
	}
}

func TestLevelFilterAndDisabled(t *testing.T) {
	if got := emit(t, "term", "warn"); strings.Contains(got, "INF") || !strings.Contains(got, "WRN request") {
		t.Fatalf("warn level:\n%s", got)
	}
	for _, dest := range []string{"", "false"} {
		l, err := Open(Options{Dest: dest}, nil, nil)
		if err != nil || l != nil {
			t.Fatalf("dest %q: %v %v", dest, l, err)
		}
		l.Access("x", tunnel.AccessEntry{}) // nil logger is a no-op
	}
	if _, err := Open(Options{Dest: "stdout", Format: "xml"}, nil, nil); err == nil {
		t.Fatal("bad format accepted")
	}
	if _, err := Open(Options{Dest: "stdout", Level: "loud"}, nil, nil); err == nil {
		t.Fatal("bad level accepted")
	}
}

func TestFileDestinationRotatesAndIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tuzy.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), MaxFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := Open(Options{Dest: path, Format: "json"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	l.Info("hello")
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != MaxFileBytes+1 {
		t.Fatalf("old log not rotated: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() == 0 || st.Size() > 200 {
		t.Fatalf("new log: %v %v", st, err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func TestTermQuotesBidiAndLineSeparators(t *testing.T) {
	for _, v := range []string{"a\u202ecod.exe", "x\u2028y", "z\u2066w"} {
		if q := quoteIfNeeded(v); !strings.HasPrefix(q, `"`) || strings.ContainsAny(q, "\u202e\u2028\u2066") {
			t.Errorf("%q → %q", v, q)
		}
	}
}
