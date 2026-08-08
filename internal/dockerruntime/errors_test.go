package dockerruntime

import (
	"errors"
	"fmt"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
)

func TestErrorPredicatesFollowTheCausalChain(t *testing.T) {
	notFound := fmt.Errorf("inspect akerdock-app: %w", cerrdefs.ErrNotFound)
	conflict := fmt.Errorf("create akerdock-app: %w", cerrdefs.ErrConflict)
	notModified := fmt.Errorf("start akerdock-app: %w", cerrdefs.ErrNotModified)
	unavailable := fmt.Errorf("server 7 channel: %w", cerrdefs.ErrUnavailable)
	plain := errors.New("connection refused")

	if !IsNotFound(notFound) || IsNotFound(conflict) || IsNotFound(plain) {
		t.Fatal("IsNotFound must match exactly the not-found chain")
	}
	if !IsConflict(conflict) || IsConflict(notFound) || IsConflict(plain) {
		t.Fatal("IsConflict must match exactly the conflict chain")
	}
	if !IsNotModified(notModified) || IsNotModified(notFound) || IsNotModified(plain) {
		t.Fatal("IsNotModified must match exactly the not-modified chain")
	}
	if !IsUnavailable(unavailable) || IsUnavailable(notFound) || IsUnavailable(plain) {
		t.Fatal("IsUnavailable must match exactly the unavailable chain")
	}
	if IsNotFound(nil) || IsConflict(nil) || IsNotModified(nil) || IsUnavailable(nil) {
		t.Fatal("nil is never a typed error")
	}
}
