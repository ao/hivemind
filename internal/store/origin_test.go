package store

import (
	"errors"
	"strings"
	"testing"
)

// A put with an origin refuses an app that exists with another origin or
// with none, and writes nothing; with Replace it takes the app over. A put
// without an origin keeps the app's. A deleted app counts as new.
func TestPutAppWithOrigin(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	shop := PutOptions{Origin: "compose:shop"}
	mustPut(t, s, web(1))
	_, err := s.PutAppWith(ctx, withEnv(1, "2"), shop)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "app web was deployed with hivemind deploy or the API, not from compose project shop") {
		t.Fatalf("put with an origin over an app without one: %v", err)
	}
	if a, _ := s.GetApp(ctx, "web"); a.Generation != 1 || a.Spec.Env["A"] != "1" || a.Origin != "" {
		t.Fatalf("web after the refused put: %+v", a)
	}

	shop.Replace = true
	if a, err := s.PutAppWith(ctx, withEnv(1, "2"), shop); err != nil || a.Origin != "compose:shop" || a.Generation != 2 {
		t.Fatalf("put with Replace: %+v, %v", a, err)
	}
	shop.Replace = false
	if a, err := s.PutAppWith(ctx, withEnv(2, "2"), shop); err != nil || a.Spec.Replicas != 2 {
		t.Fatalf("put again from the same origin: %+v, %v", a, err)
	}
	if a := mustPut(t, s, withEnv(2, "3")); a.Origin != "compose:shop" {
		t.Fatalf("a put without an origin dropped it: %+v", a)
	}
	if a, _ := s.GetApp(ctx, "web"); a.Origin != "compose:shop" {
		t.Fatalf("GetApp origin %q", a.Origin)
	}
	if _, err := s.PutAppWith(ctx, web(2), PutOptions{Origin: "compose:blog"}); !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), "deployed from compose project shop, not from compose project blog") {
		t.Fatalf("put from another origin: %v", err)
	}

	if err := s.DeleteApp(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if a, err := s.PutAppWith(ctx, web(1), PutOptions{Origin: "compose:blog"}); err != nil || a.Origin != "compose:blog" {
		t.Fatalf("put over a deleted app: %+v, %v", a, err)
	}
	if _, err := s.PutAppWith(ctx, web(1), PutOptions{Origin: "bad\norigin"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("origin with a newline: %v", err)
	}
	if _, err := s.PutAppWith(ctx, web(1), PutOptions{Origin: strings.Repeat("o", MaxOriginLen+1)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("origin too long: %v", err)
	}
	checkInvariants(t, s)
}

// A delete with an origin refuses an app that exists with another origin or
// with none, and changes nothing; without one it deletes any app. A missing
// app is ErrNotFound either way.
func TestDeleteAppWithOrigin(t *testing.T) {
	s := newT(t)
	ctx := ctxT(t)
	mustPut(t, s, web(1))
	err := s.DeleteAppWith(ctx, "web", "compose:shop")
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(),
		"app web was deployed with hivemind deploy or the API, not from compose project shop; hivemind compose delete --force deletes it anyway") {
		t.Fatalf("delete with an origin of an app without one: %v", err)
	}
	if a, err := s.GetApp(ctx, "web"); err != nil || a.Generation != 1 {
		t.Fatalf("web after the refused delete: %+v, %v", a, err)
	}
	if _, err := s.PutAppWith(ctx, web(1), PutOptions{Origin: "compose:shop", Replace: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteAppWith(ctx, "web", "compose:blog"); !errors.Is(err, ErrConflict) ||
		!strings.Contains(err.Error(), "deployed from compose project shop, not from compose project blog") {
		t.Fatalf("delete from another project: %v", err)
	}
	if err := s.DeleteAppWith(ctx, "web", "compose:shop"); err != nil {
		t.Fatalf("delete from its own project: %v", err)
	}
	if _, err := s.GetApp(ctx, "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("web after its project deleted it: %v", err)
	}
	if err := s.DeleteAppWith(ctx, "web", "compose:shop"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete of a deleted app: %v", err)
	}
	mustPut(t, s, web(1))
	if err := s.DeleteAppWith(ctx, "web", ""); err != nil {
		t.Fatalf("delete without an origin: %v", err)
	}
	if err := s.DeleteAppWith(ctx, "web", "compose:\x1b[2J"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete with a control character in the origin: %v", err)
	}
}
