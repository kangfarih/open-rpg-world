package warps

import (
	"testing"
)

func TestAtReturnsCopy(t *testing.T) {
	r := &Registry{Warps: []Warp{{ID: 1, X: 0, Y: 0, W: 10, H: 10}}}
	got := r.At(5, 5)
	if got == nil {
		t.Fatal("At(5,5) = nil, want warp")
	}
	got.X = 999
	if r.Warps[0].X == 999 {
		t.Fatal("At returned interior pointer: mutating it corrupted the registry")
	}
	if again := r.At(5, 5); again == nil || again.X != 0 {
		t.Fatalf("At after mutation = %+v, want pristine X=0", again)
	}
}

func TestAtNilRegistry(t *testing.T) {
	var r *Registry
	if got := r.At(0, 0); got != nil {
		t.Fatalf("nil At = %+v, want nil", got)
	}
}
