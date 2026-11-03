package fakeserver

import "testing"

// Who a key is: the operator for the server's own, and nobody for one it was
// never shown, or for a server that asks for no key.
func TestTheFakeSaysWhoseKeyACallCarried(t *testing.T) {
	ctx := t.Context()
	d := New(t, WithKey("admin"))
	own, err := typed(t, d, "admin").GetMeWithResponse(ctx)
	if err != nil || own.JSON200 == nil || !own.JSON200.Admin || own.JSON200.Email != operator || own.JSON200.Key == nil || len(own.JSON200.Scopes) == 0 {
		t.Errorf("the server's own key = %v %d %s, want its operator", err, own.StatusCode(), own.Body)
	}
	if other, _ := typed(t, d, "other").GetMeWithResponse(ctx); other.JSON401 == nil {
		t.Errorf("a key the server was never shown was answered %d %s, want 401", other.StatusCode(), other.Body)
	}
	// A server that asks for no key lets a call in, and has nobody to name.
	if nobody, _ := typed(t, New(t), "").GetMeWithResponse(ctx); nobody.JSON401 == nil {
		t.Errorf("a call with no key was answered %d %s, want 401", nobody.StatusCode(), nobody.Body)
	}
}
