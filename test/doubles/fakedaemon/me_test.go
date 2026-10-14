package fakedaemon

import (
	"strings"
	"testing"
)

// Who a key is: the operator for the daemon's own, and nobody for one it was
// never shown, or for a daemon that asks for no key.
func TestTheFakeSaysWhoseKeyACallCarried(t *testing.T) {
	ctx := t.Context()
	d := New(t, WithKey("admin"))
	own, err := typed(t, d, "admin").GetMeWithResponse(ctx)
	if err != nil || own.JSON200 == nil || !own.JSON200.Admin || own.JSON200.Email != operator || own.JSON200.Key == nil || len(own.JSON200.Scopes) == 0 {
		t.Errorf("the daemon's own key = %v %d %s, want its operator", err, own.StatusCode(), own.Body)
	}
	if other, _ := typed(t, d, "other").GetMeWithResponse(ctx); other.JSON401 == nil {
		t.Errorf("a key the daemon was never shown was answered %d %s, want 401", other.StatusCode(), other.Body)
	}
	// A daemon that asks for no key lets a call in, and has nobody to name.
	if nobody, _ := typed(t, New(t), "").GetMeWithResponse(ctx); nobody.JSON401 == nil {
		t.Errorf("a call with no key was answered %d %s, want 401", nobody.StatusCode(), nobody.Body)
	}
}

// What the fake says of where its key acts: bound to its first namespace, or,
// for a daemon whose keys are global, to none and reaching every namespace it
// has.
func TestTheFakeSaysWhereAKeyIsBound(t *testing.T) {
	ctx := t.Context()
	for name, c := range map[string]struct {
		opts    []Option
		bound   string
		reached []string
	}{
		"keys bound to the first namespace": {[]Option{WithKey("admin"), WithNamespaces("acme", "globex")}, "acme", []string{"acme"}},
		"the default namespace":             {[]Option{WithKey("admin")}, DefaultNamespace, []string{DefaultNamespace}},
		"global keys":                       {[]Option{WithKey("admin"), WithNamespaces("acme", "globex"), WithGlobalKeys()}, "", []string{"acme", "globex"}},
	} {
		t.Run(name, func(t *testing.T) {
			me, err := typed(t, New(t, c.opts...), "admin").GetMeWithResponse(ctx)
			if err != nil || me.JSON200 == nil || me.JSON200.Key == nil || me.JSON200.Namespaces == nil {
				t.Fatalf("me = %v %d %s", err, me.StatusCode(), me.Body)
			}
			if got := valueOf(me.JSON200.Key.Namespace); got != c.bound || strings.Join(*me.JSON200.Namespaces, ",") != strings.Join(c.reached, ",") {
				t.Errorf("bound to %q reaching %v, want %q reaching %v", got, *me.JSON200.Namespaces, c.bound, c.reached)
			}
		})
	}
}
