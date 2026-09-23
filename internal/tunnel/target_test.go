package tunnel

import "testing"

func TestParseTarget(t *testing.T) {
	ok := map[string]string{
		"3000":                       "http://localhost:3000",
		"127.0.0.1:8080":             "http://127.0.0.1:8080",
		"localhost:5173":             "http://localhost:5173",
		"http://localhost:5173":      "http://localhost:5173",
		"http://localhost:5173/app/": "http://localhost:5173/app/",
		"[::1]:9000":                 "http://[::1]:9000",
	}
	for in, want := range ok {
		u, err := ParseTarget(in)
		if err != nil || u.String() != want {
			t.Errorf("ParseTarget(%q) = %v, %v; want %s", in, u, err, want)
		}
	}
	for _, bad := range []string{"", "0", "70000", "localhost", "https://x", "file:///tmp", "ftp://x", "http://"} {
		if _, err := ParseTarget(bad); err == nil {
			t.Errorf("ParseTarget(%q) should fail", bad)
		}
	}
}
