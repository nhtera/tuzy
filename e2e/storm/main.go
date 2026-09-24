// Command storm is the pre-launch load driver (docs/ops-runbook.md "Load test").
//
//	storm seed -users 10 -names 100 -out lt/          # writes seed.sql, cleanup.sql, agents.txt
//	wrangler d1 execute tuzy --remote --file lt/seed.sql
//	storm run -server https://tuzy.dev -agents lt/agents.txt
//	# … wrangler deploy (×3, 5 min apart); storm prints one reconnect report per wave
//	wrangler d1 execute tuzy --remote --file lt/cleanup.sql
//
// `run` starts one tunnel.Client per line of agents.txt ("<token> <name>") in this process,
// serves them from a built-in local echo app, and reports initial connect latency and, for every
// reconnect wave (e.g. an edge deploy), per-agent downtime p50/p95/max, attempts and error kinds.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nhtera/tuzy/internal/tunnel"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: storm seed|run [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "seed":
		err = seed(os.Args[2:])
	case "run":
		err = run(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "storm:", err)
		os.Exit(1)
	}
}

// ───────────────────────────── seed ─────────────────────────────

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func seed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	users := fs.Int("users", 10, "test users")
	names := fs.Int("names", 100, "names per user")
	out := fs.String("out", "lt", "output directory (keep it out of git: it holds tokens)")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*out, 0o700); err != nil {
		return err
	}
	now := time.Now().Unix()
	var sql, agents strings.Builder
	sql.WriteString("-- storm seed: load-test users (lt-*), connect-scope tokens and names. Remove with cleanup.sql.\n")
	for u := 0; u < *users; u++ {
		uid := fmt.Sprintf("usr_lt%014d", u)
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		token := "tzy_" + base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256([]byte(token))
		fmt.Fprintf(&sql, "INSERT INTO users (id, email, created_at, max_names) VALUES ('%s', 'lt%02d@loadtest.invalid', %d, %d);\n", uid, u, now, *names)
		fmt.Fprintf(&sql, "INSERT INTO tokens (id, user_id, token_hash, scope, label, created_at, last_used_at) VALUES ('tok_lt%014d', '%s', '%s', 'connect', 'storm', %d, %d);\n", u, uid, hex.EncodeToString(sum[:]), now, now)
		for n := 0; n < *names; n++ {
			name := fmt.Sprintf("lt-%02d-%03d", u, n)
			def := 0
			if n == 0 {
				def = 1
			}
			fmt.Fprintf(&sql, "INSERT INTO reservations (name, user_id, gen, is_default, created_at) VALUES ('%s', '%s', '%s', %d, %d);\n", name, uid, randHex(8), def, now)
			fmt.Fprintf(&agents, "%s %s\n", token, name)
		}
	}
	cleanup := `-- storm cleanup (order matters: sessions → names → holds → tokens → users)
DELETE FROM tunnel_sessions WHERE user_id LIKE 'usr_lt%';
DELETE FROM reservations WHERE name LIKE 'lt-%';
DELETE FROM released_names WHERE name LIKE 'lt-%';
DELETE FROM usage_monthly WHERE user_id LIKE 'usr_lt%';
UPDATE tokens SET revoked_at = strftime('%s','now') WHERE user_id LIKE 'usr_lt%' AND revoked_at IS NULL;
UPDATE users SET status = 'suspended' WHERE id LIKE 'usr_lt%';
SELECT (SELECT COUNT(*) FROM reservations WHERE name LIKE 'lt-%') AS names_left;
`
	for f, c := range map[string]string{"seed.sql": sql.String(), "cleanup.sql": cleanup, "agents.txt": agents.String()} {
		if err := os.WriteFile(filepath.Join(*out, f), []byte(c), 0o600); err != nil {
			return err
		}
	}
	fmt.Printf("wrote %s/{seed.sql,cleanup.sql,agents.txt}: %d users × %d names\n", *out, *users, *names)
	fmt.Println("the KV connected markers of lt-* names expire on their own; DOs of unused names cost nothing")
	return nil
}

// ───────────────────────────── run ─────────────────────────────

type agentState struct {
	mu        sync.Mutex
	online    bool
	downSince time.Time // first loss in the current outage
	attempts  int       // dial attempts in the current outage
}

type sample struct {
	down     time.Duration
	attempts int
}

type stats struct {
	mu       sync.Mutex
	initial  []time.Duration
	wave     []sample
	errKinds map[string]int
	online   int
	total    int
}

func classify(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "cf-mitigated"), strings.Contains(s, "403"):
		return "403/cf-mitigated"
	case strings.Contains(s, "429"), strings.Contains(s, "busy"):
		return "429"
	case strings.Contains(s, "server error"), strings.Contains(s, " 5"):
		return "5xx"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return "network"
	}
	return "closed/other"
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[int(float64(len(s)-1)*p)]
}

func run(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	server := fs.String("server", "https://tuzy.dev", "edge URL")
	agentsFile := fs.String("agents", "lt/agents.txt", "lines of \"<token> <name>\"")
	limit := fs.Int("n", 0, "use only the first n agents (0 = all)")
	ramp := fs.Duration("ramp", 20*time.Millisecond, "delay between starting agents (initial connect only)")
	every := fs.Duration("report", 10*time.Second, "status line interval")
	_ = fs.Parse(args)

	srv, err := url.Parse(*server)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(*agentsFile)
	if err != nil {
		return err
	}
	var lines [][2]string
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		f := strings.Fields(l)
		if len(f) == 2 {
			lines = append(lines, [2]string{f[0], f[1]})
		}
	}
	if *limit > 0 && *limit < len(lines) {
		lines = lines[:*limit]
	}
	if len(lines) == 0 {
		return errors.New("no agents")
	}

	// Built-in local app: echo + /big?kb= for soak traffic.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n, _ := io.Copy(io.Discard, r.Body)
			fmt.Fprintf(w, "ok %s %s %d\n", r.Method, r.URL.Path, n)
		}))
	}()
	target, _ := url.Parse("http://" + ln.Addr().String())

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	st := &stats{errKinds: map[string]int{}, total: len(lines)}
	start := time.Now()
	var wg sync.WaitGroup
	fmt.Printf("storm: %d agents → %s (local app %s)\n", len(lines), srv, target)
	for i, l := range lines {
		a := &agentState{}
		began := time.Now()
		first := true
		c, err := tunnel.NewClient(tunnel.Options{
			Server: srv, Token: l[0], Name: l[1], Target: target, UserAgent: "tuzy-storm/1",
			OnEvent: func(e tunnel.Event) {
				a.mu.Lock()
				defer a.mu.Unlock()
				st.mu.Lock()
				defer st.mu.Unlock()
				switch e.Kind {
				case tunnel.EventConnecting:
					a.attempts++
				case tunnel.EventOnline:
					if first {
						st.initial = append(st.initial, time.Since(began))
						first = false
					} else if !a.downSince.IsZero() {
						st.wave = append(st.wave, sample{down: time.Since(a.downSince), attempts: a.attempts})
					}
					a.online, a.downSince, a.attempts = true, time.Time{}, 0
					st.online++
				case tunnel.EventReconnecting:
					if a.online {
						a.online = false
						st.online--
						a.downSince = time.Now()
						a.attempts = 0
					}
					if k := classify(e.Err); k != "" {
						st.errKinds[k]++
					}
				}
			},
		})
		if err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Run(ctx); err != nil {
				st.mu.Lock()
				st.errKinds["terminal: "+err.Error()]++
				st.mu.Unlock()
			}
		}()
		if i < len(lines)-1 {
			time.Sleep(*ramp)
		}
	}

	tick := time.NewTicker(*every)
	defer tick.Stop()
	lastWaveN, initialReported := 0, false
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			report(st, "final")
			return nil
		case <-tick.C:
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			st.mu.Lock()
			online, total, waveN := st.online, st.total, len(st.wave)
			st.mu.Unlock()
			fmt.Printf("%s online %d/%d  goroutines %d  heap %d MiB  wave-samples %d\n",
				time.Since(start).Round(time.Second), online, total, runtime.NumGoroutine(), ms.HeapAlloc>>20, waveN)
			// Report once everyone is back and no agent reconnected since the previous tick (a fast
			// wave can start and finish between two ticks).
			if online == total && !initialReported {
				report(st, "initial")
				initialReported = true
			}
			if online == total && waveN > 0 && waveN == lastWaveN {
				report(st, "wave")
				waveN = 0
			}
			lastWaveN = waveN
		}
	}
}

func report(st *stats, label string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	fmt.Printf("── %s report ──\n", label)
	if len(st.initial) > 0 {
		fmt.Printf("initial connect: n=%d p50=%s p95=%s max=%s\n", len(st.initial),
			pct(st.initial, .5).Round(time.Millisecond), pct(st.initial, .95).Round(time.Millisecond), pct(st.initial, 1).Round(time.Millisecond))
	}
	if len(st.wave) > 0 {
		downs := make([]time.Duration, len(st.wave))
		retried := 0
		for i, s := range st.wave {
			downs[i] = s.down
			if s.attempts > 1 {
				retried++
			}
		}
		fmt.Printf("reconnect: n=%d p50=%s p95=%s max=%s  agents needing >1 attempt: %d\n", len(downs),
			pct(downs, .5).Round(time.Millisecond), pct(downs, .95).Round(time.Millisecond), pct(downs, 1).Round(time.Millisecond), retried)
		st.wave = nil // next wave starts fresh
	}
	keys := make([]string, 0, len(st.errKinds))
	for k := range st.errKinds {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  errors[%s] = %d\n", k, st.errKinds[k])
	}
	st.initial = nil
}
