package secrets

import "testing"

func TestResolveRejectsNonOpRef(t *testing.T) {
	o := &OnePassword{}
	for _, ref := range []string{"", "hunter2", "env:FOO", "op:vault/item/field"} {
		if _, err := o.Resolve(t.Context(), ref); err == nil {
			t.Fatalf("ref %q must be rejected without contacting 1Password", ref)
		}
	}
}

func TestResolveNilReceiverFails(t *testing.T) {
	var o *OnePassword
	if _, err := o.Resolve(t.Context(), "op://fin-browser/TireRack/password"); err == nil {
		t.Fatal("nil resolver must fail closed")
	}
}
