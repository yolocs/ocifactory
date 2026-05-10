package namespace

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/yolocs/ocifactory/pkg/oci"
)

func sampleSpec() Spec {
	return Spec{
		Policy: Policy{
			Readers: []SubjectMatcher{
				{Issuer: "https://accounts.google.com", Email: "alice@example.com"},
			},
			Writers: []SubjectMatcher{
				{
					Issuer:   "https://token.actions.githubusercontent.com",
					SubMatch: "repo:org/repo:*",
					Kind:     "oidc",
				},
			},
		},
	}
}

func TestStore_PutGet_Roundtrip(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	ns := &Namespace{Name: "myteam", Spec: sampleSpec()}
	if err := s.Put(ctx, ns); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := s.Get(ctx, "myteam")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(ns, got); diff != "" {
		t.Errorf("Get mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_Put_Updates(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	first := &Namespace{Name: "myteam", Spec: sampleSpec()}
	if err := s.Put(ctx, first); err != nil {
		t.Fatalf("Put first: %v", err)
	}

	updated := &Namespace{
		Name: "myteam",
		Spec: Spec{
			Policy: Policy{
				Readers: []SubjectMatcher{
					{Issuer: "https://accounts.google.com", Email: "bob@example.com"},
				},
			},
		},
	}
	if err := s.Put(ctx, updated); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	got, err := s.Get(ctx, "myteam")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if diff := cmp.Diff(updated, got); diff != "" {
		t.Errorf("Get mismatch after update (-want +got):\n%s", diff)
	}

	// The index entry must remain a single tag — the second Put
	// must not stack a duplicate sentinel.
	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if diff := cmp.Diff([]string{"myteam"}, names); diff != "" {
		t.Errorf("List mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	_, err := s.Get(ctx, "missing")
	if err == nil {
		t.Fatal("Get(missing) = nil, want error")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(missing) error %v, want errors.Is ErrNotFound", err)
	}
}

func TestStore_Get_InvalidName(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	_, err := s.Get(ctx, "_internal")
	if !errors.Is(err, ErrInvalidName) {
		t.Errorf("Get(_internal) error %v, want errors.Is ErrInvalidName", err)
	}
}

func TestStore_Put_InvalidName(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	if err := s.Put(ctx, &Namespace{Name: "BadName", Spec: sampleSpec()}); !errors.Is(err, ErrInvalidName) {
		t.Errorf("Put error %v, want errors.Is ErrInvalidName", err)
	}
}

func TestStore_Put_NilNamespace(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	if err := s.Put(ctx, nil); err == nil {
		t.Fatal("Put(nil) = nil, want error")
	}
}

func TestStore_List_Empty(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List on empty store: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List on empty store = %v, want empty", names)
	}
}

func TestStore_List_ReflectsCRUD(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		if err := s.Put(ctx, &Namespace{Name: name, Spec: sampleSpec()}); err != nil {
			t.Fatalf("Put %q: %v", name, err)
		}
	}

	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	slices.Sort(names)
	if diff := cmp.Diff([]string{"alpha", "beta", "gamma"}, names); diff != "" {
		t.Errorf("List mismatch (-want +got):\n%s", diff)
	}

	if err := s.Delete(ctx, "beta"); err != nil {
		t.Fatalf("Delete beta: %v", err)
	}

	names, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	slices.Sort(names)
	if diff := cmp.Diff([]string{"alpha", "gamma"}, names); diff != "" {
		t.Errorf("List mismatch after delete (-want +got):\n%s", diff)
	}
}

func TestStore_Delete_Removes(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	if err := s.Put(ctx, &Namespace{Name: "myteam", Spec: sampleSpec()}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "myteam"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get(ctx, "myteam")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete error %v, want errors.Is ErrNotFound", err)
	}

	names, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List after Delete: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("List after Delete = %v, want empty", names)
	}
}

func TestStore_Delete_NotFound(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	if err := s.Delete(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete(missing) error %v, want errors.Is ErrNotFound", err)
	}
}

func TestStore_WithPrefix_RoutesToConfiguredRepos(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake, WithPrefix("custom/_namespaces"))
	ctx := t.Context()

	if err := s.Put(ctx, &Namespace{Name: "myteam", Spec: sampleSpec()}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Per-namespace metadata lives at <prefix>/<name>:_metadata.
	if _, ok := fake.Files["custom/_namespaces/myteam/_metadata/"+specFileName]; !ok {
		t.Errorf("expected metadata file under custom/_namespaces/myteam/_metadata, got files: %v", keys(fake.Files))
	}
	// Index lives at <prefix>/_index, one tag per namespace.
	indexTags, err := fake.ListTags(ctx, "custom/_namespaces/_index")
	if err != nil {
		t.Fatalf("ListTags index: %v", err)
	}
	if diff := cmp.Diff([]string{"myteam"}, indexTags); diff != "" {
		t.Errorf("index tags mismatch (-want +got):\n%s", diff)
	}
}

func TestStore_OnDiskLayout(t *testing.T) {
	t.Parallel()

	fake := oci.NewFakeRegistry()
	s := NewStore(fake)
	ctx := t.Context()

	want := sampleSpec()
	if err := s.Put(ctx, &Namespace{Name: "myteam", Spec: want}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Pin the metadata path so a future refactor that breaks the
	// on-disk shape trips this test rather than silently migrating
	// every operator's existing storage.
	body, ok := fake.Files["_namespaces/myteam/_metadata/"+specFileName]
	if !ok {
		t.Fatalf("metadata file not found, got files: %v", keys(fake.Files))
	}
	var got Spec
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("Unmarshal metadata: %v", err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("on-disk spec mismatch (-want +got):\n%s", diff)
	}

	// And the index sentinel.
	if _, ok := fake.Files["_namespaces/_index/myteam/"+indexSentinelName]; !ok {
		t.Errorf("expected index sentinel file, got files: %v", keys(fake.Files))
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
