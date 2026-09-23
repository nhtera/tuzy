package cli

import (
	"context"
	"errors"
	"net/url"
	"sync"

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

// runTunnels runs every tunnel in one process until ctx is cancelled (graceful drain → nil) or one
// of them fails terminally (all stop, that error is returned).
func runTunnels(ctx context.Context, env *runtimeEnv, specs []tunnelSpec, p *ui.Printer) error {
	if env.token == "" {
		return errors.New("not logged in: run `tuzy login` (or set TUZY_TOKEN in CI)")
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	multi := len(specs) > 1

	// Build every client first so a config error never leaves some tunnels running.
	clients := make([]*tunnel.Client, 0, len(specs))
	for _, s := range specs {
		label := ""
		if multi {
			label = s.name
		}
		target := s.target.String()
		c, err := tunnel.NewClient(tunnel.Options{
			Server:     env.server,
			Token:      env.token,
			Name:       s.name,
			Force:      s.force,
			Target:     s.target,
			HostHeader: s.hostHeader,
			UserAgent:  userAgent(),
			OnEvent:    func(e tunnel.Event) { p.Event(e, target) },
			OnAccess:   func(a tunnel.AccessEntry) { p.Access(label, a) },
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
