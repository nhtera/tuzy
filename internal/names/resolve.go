package names

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/nhtera/tuzy/internal/api"
)

// API is the subset of the edge API the resolver needs.
type API interface {
	ListNames(ctx context.Context, live bool) (api.NameList, error)
	SuggestName(ctx context.Context) (string, error)
	AddName(ctx context.Context, name string) (api.Name, error)
}

// Resolver picks the tunnel name for `tuzy http` / `tuzy start`:
//
//  1. an explicit name: use it if owned; claim it if free (never inside the reconnect loop);
//     refuse names on hold for you (reclaim explicitly with `tuzy names add`);
//  2. otherwise the server-side default;
//  3. with no names at all: prompt on a terminal (suggestion pre-filled), else auto-generate.
type Resolver struct {
	API         API
	Interactive bool
	Ask         func(question string) (string, error)
	Out         io.Writer
}

// Resolve returns the name to connect with.
func (r *Resolver) Resolve(ctx context.Context, explicit string) (string, error) {
	list, err := r.API.ListNames(ctx, false)
	if err != nil {
		return "", err
	}
	if explicit != "" {
		return r.explicit(ctx, list, explicit)
	}
	for _, n := range list.Names {
		if n.Default {
			return n.Name, nil
		}
	}
	if len(list.Names) > 0 { // invariant says there is always a default; be lenient
		return list.Names[0].Name, nil
	}
	return r.firstName(ctx)
}

func (r *Resolver) explicit(ctx context.Context, list api.NameList, name string) (string, error) {
	for _, n := range list.Names {
		if n.Name == name {
			return name, nil
		}
	}
	for _, h := range list.Held {
		if h.Name == name {
			if h.RenamedTo != nil {
				return "", fmt.Errorf("`%s` was renamed to `%s`; use --name %s (or reclaim the old name with `tuzy names add %s`)", name, *h.RenamedTo, *h.RenamedTo, name)
			}
			return "", fmt.Errorf("`%s` is on hold for you after you released it; run `tuzy names add %s` to reclaim it", name, name)
		}
	}
	n, err := r.API.AddName(ctx, name)
	if err != nil {
		return "", scopeHint(err)
	}
	fmt.Fprintf(r.Out, "Reserved new name %s (%d/%d)\n", n.Name, list.Used+1, list.Limit)
	return n.Name, nil
}

func (r *Resolver) firstName(ctx context.Context) (string, error) {
	if !r.Interactive || r.Ask == nil {
		return r.autoName(ctx)
	}
	fmt.Fprintln(r.Out, "Pick a permanent subdomain for your tunnels (you can have up to 10).")
	for attempt := 0; attempt < 5; attempt++ {
		suggestion, err := r.API.SuggestName(ctx)
		if err != nil {
			return "", err
		}
		answer, err := r.Ask(fmt.Sprintf("Choose your subdomain [%s]: ", suggestion))
		if err != nil {
			return r.autoName(ctx) // no input after all (stdin closed): don't block the tunnel
		}
		if answer == "" {
			answer = suggestion
		}
		n, err := r.API.AddName(ctx, answer)
		var ae *api.Error
		if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 409) {
			fmt.Fprintf(r.Out, "  %s\n", ae.Message)
			continue
		}
		if err != nil {
			return "", scopeHint(err)
		}
		fmt.Fprintf(r.Out, "Reserved %s\n", n.Name)
		return n.Name, nil
	}
	return "", errors.New("no name chosen; run `tuzy names add <name>`")
}

func (r *Resolver) autoName(ctx context.Context) (string, error) {
	n, err := r.API.AddName(ctx, "")
	if err != nil {
		return "", scopeHint(err)
	}
	fmt.Fprintf(r.Out, "Reserved %s — it stays yours; change it with `tuzy names rename`\n", n.Name)
	return n.Name, nil
}

// scopeHint explains why a connect-scope (CI) token can't reserve names.
func scopeHint(err error) error {
	var ae *api.Error
	if errors.As(err, &ae) && ae.Code == "insufficient_scope" {
		return fmt.Errorf("this token can only connect to names you already own; reserve the name with `tuzy names add` from a logged-in machine, then pass --name: %w", err)
	}
	return err
}
