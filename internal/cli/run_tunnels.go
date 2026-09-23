package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"

	"github.com/nhtera/tuzy/internal/inspector"
	"github.com/nhtera/tuzy/internal/tunnel"
	"github.com/nhtera/tuzy/internal/ui"
)

// tunnelSpec is one tunnel to run.
type tunnelSpec struct {
	name       string
	target     *url.URL
	hostHeader string
	force      bool
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
func runTunnels(ctx context.Context, env *runtimeEnv, specs []tunnelSpec, p *ui.Printer, iopts inspectOpts, out io.Writer) error {
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
	for _, s := range specs {
		label := ""
		if multi {
			label = s.name
		}
		target := s.target.String()
		spec := s
		onEvent := func(e tunnel.Event) {
			p.Event(e, target)
			if store == nil {
				return
			}
			switch e.Kind {
			case tunnel.EventOnline:
				// The one source of truth for a tunnel's name: what READY confirmed the session is
				// serving. Begin() (inspector.Store) uses this same name (the session's), so
				// entries and this TunnelInfo always agree.
				store.SetTunnel(inspector.TunnelInfo{Name: e.Name, PublicURL: e.URL, Target: spec.target, HostHeader: spec.hostHeader})
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
			Server:         env.server,
			Token:          env.token,
			Name:           s.name,
			Force:          s.force,
			Target:         s.target,
			HostHeader:     s.hostHeader,
			UserAgent:      userAgent(),
			OnEvent:        onEvent,
			OnAccess:       func(a tunnel.AccessEntry) { p.Access(label, a) },
			Recorder:       rec,
			LocalTransport: rt,
		})
		if err != nil {
			return err
		}
		clients = append(clients, c)
	}
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.Run(ctx); err != nil {
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
