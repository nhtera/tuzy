package names

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nhtera/tuzy/internal/api"
)

type fakeAPI struct {
	list    api.NameList
	taken   map[string]bool
	added   []string
	suggest string
}

func (f *fakeAPI) ListNames(context.Context, bool) (api.NameList, error) { return f.list, nil }
func (f *fakeAPI) SuggestName(context.Context) (string, error)           { return f.suggest, nil }
func (f *fakeAPI) AddName(_ context.Context, name string) (api.Name, error) {
	if name == "" {
		name = "auto-name-42"
	}
	if f.taken[name] {
		return api.Name{}, &api.Error{Status: 409, Code: "name_taken", Message: "that name is taken"}
	}
	f.added = append(f.added, name)
	return api.Name{Name: name}, nil
}

func TestResolveDefault(t *testing.T) {
	f := &fakeAPI{list: api.NameList{Names: []api.Name{{Name: "a"}, {Name: "shop", Default: true}}}}
	r := &Resolver{API: f, Out: &bytes.Buffer{}}
	if n, err := r.Resolve(context.Background(), ""); err != nil || n != "shop" {
		t.Fatalf("got %q %v", n, err)
	}
	if len(f.added) != 0 {
		t.Fatal("default path must not claim")
	}
}

func TestResolveExplicitOwnedFreeHeld(t *testing.T) {
	f := &fakeAPI{list: api.NameList{Names: []api.Name{{Name: "shop", Default: true}}, Used: 1, Limit: 10, Held: []api.HeldName{{Name: "old"}}}}
	var out bytes.Buffer
	r := &Resolver{API: f, Out: &out}
	if n, _ := r.Resolve(context.Background(), "shop"); n != "shop" || len(f.added) != 0 {
		t.Fatal("owned name must not be claimed again")
	}
	if n, err := r.Resolve(context.Background(), "fresh"); err != nil || n != "fresh" || !strings.Contains(out.String(), "Reserved new name fresh (2/10)") {
		t.Fatalf("free: %q %v %q", n, err, out.String())
	}
	if _, err := r.Resolve(context.Background(), "old"); err == nil || !strings.Contains(err.Error(), "tuzy names add old") {
		t.Fatalf("held: %v", err)
	}
}

func TestFirstRunNonInteractiveAutoNames(t *testing.T) {
	f := &fakeAPI{}
	r := &Resolver{API: f, Out: &bytes.Buffer{}}
	if n, err := r.Resolve(context.Background(), ""); err != nil || n != "auto-name-42" {
		t.Fatalf("got %q %v", n, err)
	}
}

func TestFirstRunPromptRetriesTakenAndAcceptsSuggestion(t *testing.T) {
	f := &fakeAPI{taken: map[string]bool{"taken-one": true}, suggest: "brave-otter-42"}
	answers := []string{"taken-one", ""}
	var out bytes.Buffer
	r := &Resolver{API: f, Out: &out, Interactive: true, Ask: func(string) (string, error) {
		a := answers[0]
		answers = answers[1:]
		return a, nil
	}}
	n, err := r.Resolve(context.Background(), "")
	if err != nil || n != "brave-otter-42" {
		t.Fatalf("got %q %v", n, err)
	}
	if !strings.Contains(out.String(), "that name is taken") {
		t.Fatalf("output %q", out.String())
	}
}
