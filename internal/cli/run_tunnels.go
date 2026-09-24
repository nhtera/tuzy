package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/nhtera/tuzy/internal/config"
	"github.com/nhtera/tuzy/internal/fileserver"
	"github.com/nhtera/tuzy/internal/inspector"
	"github.com/nhtera/tuzy/internal/logging"
	"github.com/nhtera/tuzy/internal/tunnel"
	"github.com/nhtera/tuzy/internal/ui"
	"github.com/nhtera/tuzy/internal/upstream"
)

// tunnelSpec is one tunnel to run.
type tunnelSpec struct {
	name       string
	target     *url.URL
	hostHeader string
	force      bool
	// display is the human/log-facing form of target: identical to target.String() except for a
	// file:// target, where it names the actual served directory (target itself is only the
	// fileTargetScheme placeholder tunnel.FileTarget() — see buildTarget).
	display string
	// transport is this tunnel's own local transport: nil for a plain http target (the shared
	// default is used), an https TLS clone, or a fileserver.RoundTripper for a file:// target.
	transport http.RoundTripper
}

// buildTarget resolves one addr (a `tuzy http` argument or a tuzy.toml tunnels.<name>.addr) into a
// tunnelSpec's target + transport. resolveDir is how a file:// target's directory is validated:
// upstream.ResolveCLIDir for a command-line addr, upstream.ResolveProjectDir (bound to the project
// directory) for tuzy.toml — see the SECURITY note on ResolveProjectDir. Any startup message (the
// served path, an insecure-TLS warning) is written to out: callers must pass a writer that never
// interleaves with a structured (e.g. JSON) log stream — see logFlags.output.
//
// tlsOpts (--upstream-sni / --upstream-insecure, or a tuzy.toml entry's upstream_sni /
// upstream_insecure) only means anything for an https:// target; using it with any other kind is
// rejected rather than silently ignored.
func buildTarget(addr string, tlsOpts upstream.TLSOptions, resolveDir func(string) (string, error), out io.Writer) (target *url.URL, display string, transport http.RoundTripper, err error) {
	t, err := upstream.Parse(addr)
	if err != nil {
		return nil, "", nil, err
	}
	if t.Kind != upstream.KindHTTPS && (tlsOpts.SNI != "" || tlsOpts.Insecure) {
		return nil, "", nil, fmt.Errorf("--upstream-sni / --upstream-insecure (upstream_sni / upstream_insecure) only apply to an https:// target, not %s: %q", t.Kind, addr)
	}
	switch t.Kind {
	case upstream.KindFile:
		dir, err := resolveDir(t.Dir)
		if err != nil {
			return nil, "", nil, err
		}
		rt, err := fileserver.New(dir)
		if err != nil {
			return nil, "", nil, fmt.Errorf("file target %q: %w", t.Dir, err)
		}
		fmt.Fprintf(out, "serving files from %s (read-only, dotfiles hidden)\n", dir)
		return tunnel.FileTarget(), "file://" + dir, rt, nil
	case upstream.KindHTTPS:
		if tlsOpts.Insecure {
			fmt.Fprintln(out, "warning: --upstream-insecure disables TLS certificate verification for the upstream")
		}
		return t.URL, t.URL.String(), upstream.NewHTTPSTransport(tlsOpts), nil
	default:
		return t.URL, t.URL.String(), nil, nil // nil: the shared default local transport is used
	}
}

// buildCLITarget resolves a `tuzy http`/positional-arg addr: a file:// dir resolves against the
// current working directory (an absolute file:/// path is allowed here — see
// upstream.ResolveCLIDir).
func buildCLITarget(addr string, tlsOpts upstream.TLSOptions, out io.Writer) (target *url.URL, display string, transport http.RoundTripper, err error) {
	return buildTarget(addr, tlsOpts, upstream.ResolveCLIDir, out)
}

// buildProjectTarget resolves a tuzy.toml tunnels.<name>.addr: a file:// dir must be a relative
// path resolving inside projectFile's directory (SECURITY: see upstream.ResolveProjectDir).
func buildProjectTarget(projectFile, addr string, tlsOpts upstream.TLSOptions, out io.Writer) (target *url.URL, display string, transport http.RoundTripper, err error) {
	absFile, err := filepath.Abs(projectFile)
	if err != nil {
		return nil, "", nil, err
	}
	projectDir := filepath.Dir(absFile)
	resolveDir := func(dir string) (string, error) { return upstream.ResolveProjectDir(projectDir, dir) }
	return buildTarget(addr, tlsOpts, resolveDir, out)
}

// tunnelTLS reads the optional per-tunnel upstream_insecure / upstream_sni keys of a tuzy.toml
// entry.
func tunnelTLS(t config.Tunnel) upstream.TLSOptions {
	return upstream.TLSOptions{SNI: t.UpstreamSNI, Insecure: t.UpstreamInsecure}
}

// inspectOpts configures the local inspector shared by every tunnel of the process.
type inspectOpts struct {
	disabled bool
	addr     string // default 127.0.0.1:4040 (falls back to 4041–4049)
}

// startInspector starts the shared inspector; failures only warn (tunnels work without it).
func startInspector(o inspectOpts, env *runtimeEnv, rt http.RoundTripper, out io.Writer) (*inspector.Store, func()) {
	if o.disabled {
		return nil, func() {}
	}
	addr := o.addr
	if addr == "" {
		addr = env.user.InspectAddr
	}
	if addr == "" {
		addr = "127.0.0.1:4040"
	}
	store := inspector.NewStore()
	srv, err := inspector.Listen(addr, store, rt)
	if err != nil {
		fmt.Fprintf(out, "note: inspector disabled (%v); use --inspect-addr to pick a port\n", err)
		return nil, func() {}
	}
	fmt.Fprintf(out, "inspector: %s\n", srv.URL())
	return store, func() { _ = srv.Close() }
}

// runTunnels runs every tunnel in one process until ctx is cancelled (graceful drain → nil) or one
// of them fails terminally (all stop, that error is returned).
func runTunnels(ctx context.Context, env *runtimeEnv, specs []tunnelSpec, p *ui.Printer, lg *logging.Logger, iopts inspectOpts, out io.Writer) error {
	defer func() {
		for _, s := range specs {
			closeTransport(s.transport)
		}
	}()
	if env.token == "" {
		return errors.New("not logged in: run `tuzy login` (or set TUZY_TOKEN in CI)")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	multi := len(specs) > 1
	rt := tunnel.NewLocalTransport()
	store, stopInspector := startInspector(iopts, env, rt, out)
	defer stopInspector()

	// Build every client first so a config error never leaves some tunnels running.
	clients := make([]*tunnel.Client, 0, len(specs))
	names := make([]string, 0, len(specs)) // parallel to clients, for the terminal-error log below
	for _, s := range specs {
		label := ""
		if multi {
			label = s.name
		}
		target := s.display
		spec := s
		transport := s.transport
		if transport == nil {
			transport = rt // plain http target: the shared default local transport
		}
		onEvent := func(e tunnel.Event) {
			p.Event(e, target)
			lg.Event(e, target)
			if store == nil {
				return
			}
			switch e.Kind {
			case tunnel.EventOnline:
				// The one source of truth for a tunnel's name: what READY confirmed the session is
				// serving. Begin() (inspector.Store) uses this same name (the session's), so
				// entries and this TunnelInfo always agree.
				store.SetTunnel(inspector.TunnelInfo{Name: e.Name, PublicURL: e.URL, Target: spec.target, HostHeader: spec.hostHeader, Transport: transport})
			case tunnel.EventRenamed:
				// Relabel immediately (don't wait for the reconnect's EventOnline) so entries
				// recorded under the new name — which the next session serves as soon as it is
				// online — always find a matching TunnelInfo. Replay of entries recorded under the
				// old name still resolves: the old mapping is left in place (the local target does
				// not change on a rename).
				if t, ok := store.Tunnel(e.OldName); ok {
					t.Name = e.Name
					store.SetTunnel(t)
				}
			}
		}
		var rec tunnel.Recorder
		if store != nil {
			rec = store
		}
		c, err := tunnel.NewClient(tunnel.Options{
			Server:     env.server,
			Token:      env.token,
			Name:       s.name,
			Force:      s.force,
			Target:     s.target,
			HostHeader: s.hostHeader,
			UserAgent:  userAgent(),
			OnEvent:    onEvent,
			OnAccess: func(a tunnel.AccessEntry) {
				p.Access(label, a)
				lg.Access(s.name, a)
			},
			Recorder:       rec,
			LocalTransport: transport,
		})
		if err != nil {
			return err
		}
		clients = append(clients, c)
		names = append(names, s.name)
	}
	var wg sync.WaitGroup
	for i, c := range clients {
		wg.Add(1)
		name := names[i]
		go func() {
			defer wg.Done()
			if err := c.Run(ctx); err != nil {
				lg.Error("tunnel.error", "name", name, "error", err.Error())
				cancel(err)
			}
		}()
	}
	wg.Wait()
	if err := context.Cause(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// logFlags are --log, --log-format and --log-level (defaults from the user config).
type logFlags struct{ dest, format, level string }

func (l *logFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&l.dest, "log", "", "write structured logs to stdout, stderr or a file (false: off)")
	cmd.Flags().StringVar(&l.format, "log-format", "", "log format: term, logfmt or json")
	cmd.Flags().StringVar(&l.level, "log-level", "", "log level: debug, info, warn or error")
}

// output opens the logger and the human printer, and picks the writer any other startup notice
// (buildTarget's "serving files from …" / --upstream-insecure warning, startInspector's
// "inspector: …") must use: cmd.OutOrStdout() normally, but cmd.ErrOrStderr() when --log (or the
// user config's log) sends structured logs to stdout — a plain line there would corrupt e.g. a JSON
// log stream a caller is parsing line by line. Must be called (this resolves the log destination)
// before buildTarget/startInspector run.
//
// Logging to stdout also replaces the human status/access-log output (same reason: don't interleave
// it with a JSON stream).
func (l logFlags) output(cmd *cobra.Command, env *runtimeEnv, quiet bool) (p *ui.Printer, lg *logging.Logger, notices io.Writer, err error) {
	pick := func(flag, cfg string) string {
		if flag != "" {
			return flag
		}
		return cfg
	}
	opts := logging.Options{Dest: pick(l.dest, env.user.Log), Format: pick(l.format, env.user.LogFormat), Level: pick(l.level, env.user.LogLevel)}
	if !opts.Enabled() && (l.format != "" || l.level != "") {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: --log-format / --log-level have no effect without --log")
	}
	lg, err = logging.Open(opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return nil, nil, nil, err
	}
	human := cmd.OutOrStdout()
	notices = cmd.OutOrStdout()
	if opts.Dest == "stdout" {
		human = io.Discard
		notices = cmd.ErrOrStderr()
	}
	return ui.New(human, quiet, env.displayServer()), lg, notices, nil
}

// closeTransport releases per-tunnel resources (e.g. the file server's directory handle, which
// Windows won't let anyone delete while it is open). Safe to call more than once.
func closeTransport(rt http.RoundTripper) {
	if c, ok := rt.(io.Closer); ok {
		_ = c.Close()
	}
}
