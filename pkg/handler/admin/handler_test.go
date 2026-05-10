package admin_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/yolocs/ocifactory/pkg/handler/admin"
	"github.com/yolocs/ocifactory/pkg/namespace"
	"github.com/yolocs/ocifactory/pkg/oci"
)

func TestHandler_NamespaceCRUD(t *testing.T) {
	t.Parallel()

	allowAlice := namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: "alice@example.com"}}}}
	allowBob := namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{Email: "bob@example.com"}}}}

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		seed       func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store)
		wantStatus int
		wantBody   any
	}{
		{
			name:       "put new namespace",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/alpha",
			body:       allowAlice,
			wantStatus: http.StatusCreated,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowAlice},
		},
		{
			name:   "put existing namespace",
			method: http.MethodPut,
			path:   "/admin/v1/namespaces/alpha",
			body:   allowBob,
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", allowAlice)
			},
			wantStatus: http.StatusOK,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowBob},
		},
		{
			name:       "put invalid name",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/Bad_Name",
			body:       namespace.Spec{},
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid namespace name: \"Bad_Name\" contains invalid character 'B'"},
		},
		{
			name:       "put invalid spec",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badspec",
			body:       namespace.Spec{Policy: namespace.Policy{Readers: []namespace.SubjectMatcher{{}}}},
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "readers[0]: invalid policy: matcher must populate at least one field"},
		},
		{
			name:   "get existing namespace",
			method: http.MethodGet,
			path:   "/admin/v1/namespaces/alpha",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", allowBob)
			},
			wantStatus: http.StatusOK,
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowBob},
		},
		{
			name:       "get unknown namespace",
			method:     http.MethodGet,
			path:       "/admin/v1/namespaces/missing",
			wantStatus: http.StatusNotFound,
			wantBody:   map[string]string{"error": "namespace not found: missing"},
		},
		{
			name:   "list namespaces",
			method: http.MethodGet,
			path:   "/admin/v1/namespaces",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", namespace.Spec{})
				putNamespaceNow(t, store, "beta", namespace.Spec{})
			},
			wantStatus: http.StatusOK,
			wantBody:   map[string][]string{"namespaces": []string{"alpha", "beta"}},
		},
		{
			name:   "delete non-empty namespace",
			method: http.MethodDelete,
			path:   "/admin/v1/namespaces/alpha",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "alpha", namespace.Spec{})
				reg.AddTag(path.Join("alpha", "ocifactory-packages"), hex.EncodeToString([]byte("pkg")))
			},
			wantStatus: http.StatusConflict,
			wantBody:   map[string]string{"error": "namespace is not empty"},
		},
		{
			name:   "delete empty namespace",
			method: http.MethodDelete,
			path:   "/admin/v1/namespaces/beta",
			seed: func(t *testing.T, reg *oci.FakeRegistry, store *namespace.Store) {
				t.Helper()
				putNamespaceNow(t, store, "beta", namespace.Spec{})
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "delete unknown namespace",
			method:     http.MethodDelete,
			path:       "/admin/v1/namespaces/beta",
			wantStatus: http.StatusNotFound,
			wantBody:   map[string]string{"error": "namespace not found: beta"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := oci.NewFakeRegistry()
			store := namespace.NewStore(reg)
			nsReg := namespace.NewRegistry(reg, store)
			h, err := admin.NewHandler(store, nsReg)
			if err != nil {
				t.Fatalf("NewHandler: %v", err)
			}
			srv := httptest.NewServer(h.Mux())
			t.Cleanup(srv.Close)

			if tc.seed != nil {
				tc.seed(t, reg, store)
			}
			var body []byte
			if tc.body != nil {
				var err error
				body, err = json.Marshal(tc.body)
				if err != nil {
					t.Fatalf("Marshal body: %v", err)
				}
			}
			req, err := http.NewRequestWithContext(t.Context(), tc.method, srv.URL+tc.path, bytes.NewReader(body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if diff := cmp.Diff(tc.wantStatus, resp.StatusCode); diff != "" {
				t.Fatalf("status mismatch (-want +got):\n%s", diff)
			}
			if tc.wantBody == nil {
				return
			}
			assertJSONBody(t, resp, tc.wantBody)
		})
	}
}

func TestNewHandler_RequiresDependencies(t *testing.T) {
	t.Parallel()

	reg := oci.NewFakeRegistry()
	store := namespace.NewStore(reg)
	nsReg := namespace.NewRegistry(reg, store)

	tests := []struct {
		name      string
		store     admin.Store
		packages  admin.PackageLister
		wantError string
	}{
		{name: "missing store", packages: nsReg, wantError: "store is required"},
		{name: "missing package lister", store: store, wantError: "package lister is required"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := admin.NewHandler(tc.store, tc.packages)
			if diff := cmp.Diff(tc.wantError, errString(err)); diff != "" {
				t.Errorf("NewHandler() error mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func putNamespaceNow(t *testing.T, store *namespace.Store, name string, spec namespace.Spec) {
	t.Helper()
	if err := store.Put(t.Context(), &namespace.Namespace{Name: name, Spec: spec}); err != nil {
		t.Fatalf("Store.Put(%q): %v", name, err)
	}
}

func assertJSONBody(t *testing.T, resp *http.Response, want any) {
	t.Helper()
	if got, want := resp.Header.Get("Content-Type"), "application/json"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	switch want := want.(type) {
	case *namespace.Namespace:
		var got namespace.Namespace
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode namespace: %v", err)
		}
		if diff := cmp.Diff(*want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	case map[string]string:
		var got map[string]string
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode error body: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	case map[string][]string:
		var got map[string][]string
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("Decode list body: %v", err)
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("body mismatch (-want +got):\n%s", diff)
		}
	default:
		t.Fatalf("unsupported want body type %T", want)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
