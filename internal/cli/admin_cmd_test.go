package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAdminCommandsHitTheAdminAPI(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path+" "+r.URL.RawQuery+" "+toJSON(in))
		mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/admin/reports":
			_, _ = w.Write([]byte(`{"reports":[{"id":"rpt_1","name":"evil","category":"phishing","details":"fake\u001b[31m bank","reporter_prefix":"203.0.113.0/24","status":"open","created_at":1800000000,"name_status":"active"}]}`))
		default:
			_, _ = w.Write([]byte(`{"changed":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TUZY_TOKEN", "tzy_admin")
	t.Setenv("TUZY_SERVER", srv.URL)

	out, err := run(t, "", "admin", "reports")
	if err != nil {
		t.Fatalf("reports: %v\n%s", err, out)
	}
	if !strings.Contains(out, "rpt_1") || strings.Contains(out, "\x1b") {
		t.Fatalf("reports output must list the report with control characters neutralized:\n%q", out)
	}
	for _, args := range [][]string{
		{"admin", "suspend-name", "Evil", "--reason", "phishing kit"},
		{"admin", "untrust-user", "a@b.c"},
		{"admin", "report", "rpt_1", "actioned"},
		{"admin", "set-max-names", "usr_1", "3"},
		{"admin", "reserve", "usr_1", "VIP"},
		{"admin", "set-max-names", "--new-accounts", "2"},
	} {
		if out, err := run(t, "", args...); err != nil || !strings.Contains(out, `"changed": true`) {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	if _, err := run(t, "", "admin", "report", "rpt_1", "maybe"); err == nil {
		t.Fatal("report with a bad status must fail locally")
	}
	want := []string{
		"GET /api/v1/admin/reports status=open null",
		`POST /api/v1/admin/names/evil/suspend  {"reason":"phishing kit"}`,
		"POST /api/v1/admin/users/a@b.c/untrust  {}",
		`POST /api/v1/admin/reports/rpt_1  {"status":"actioned"}`,
		`POST /api/v1/admin/users/usr_1/max-names  {"max":3}`,
		`POST /api/v1/admin/names/vip/reserve  {"user":"usr_1"}`,
		`POST /api/v1/admin/settings/new-account-max-names  {"max":2}`,
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

func TestWhoamiShowsLongStreamUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"user":{"id":"usr_1","email":"a@b.c"},"token":{"id":"tok_1","scope":"full","label":"mbp"},"usage":{"month":"2026-09","long_stream_seconds":45000,"long_stream_budget_seconds":180000}}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TUZY_TOKEN", "tzy_x")
	t.Setenv("TUZY_SERVER", srv.URL)
	out, err := run(t, "", "whoami")
	if err != nil || !strings.Contains(out, "long streams: 12.5 h of 50.0 h this month (2026-09)") {
		t.Fatalf("%v\n%s", err, out)
	}
}

func toJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestServiceInstallDryRunRendersTheUnit(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "tuzy.toml")
	if err := os.WriteFile(proj, []byte("[tunnels.web]\naddr = \"3000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "", "service", "install", "--dry-run", "--file", proj)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"start", "--all", proj, "--log-format", "json"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
}
