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

	// Admin PUT stamps CurrentSchemaVersion via Spec.Normalize before
	// persisting, so the response body — and any subsequent GET —
	// carries it back. allowAlicePersisted / allowBobPersisted are the
	// wantBody shape after that normalization.
	allowAlicePersisted := allowAlice
	allowAlicePersisted.SchemaVersion = namespace.CurrentSchemaVersion
	allowBobPersisted := allowBob
	allowBobPersisted.SchemaVersion = namespace.CurrentSchemaVersion

	tests := []struct {
		name       string
		method     string
		path       string
		body       any
		rawBody    string
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
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowAlicePersisted},
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
			wantBody:   &namespace.Namespace{Name: "alpha", Spec: allowBobPersisted},
		},
		{
			name:       "put rejects future schema version",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/futureversion",
			rawBody:    `{"schema_version":999}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "unsupported namespace schema_version 999 (this ocifactory understands up to 1)"},
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
			name:   "put proxy namespace",
			method: http.MethodPut,
			path:   "/admin/v1/namespaces/pypi-proxy",
			body: namespace.Spec{
				Mode:  namespace.ModeProxy,
				Proxy: namespace.Proxy{Upstream: "https://pypi.org"},
			},
			wantStatus: http.StatusCreated,
			wantBody: &namespace.Namespace{
				Name: "pypi-proxy",
				Spec: namespace.Spec{
					SchemaVersion: namespace.CurrentSchemaVersion,
					Mode:          namespace.ModeProxy,
					Proxy:         namespace.Proxy{Upstream: "https://pypi.org"},
				},
			},
		},
		{
			name:       "put proxy without upstream",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badproxy",
			rawBody:    `{"mode":"proxy"}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid proxy: upstream is required on mode \"proxy\""},
		},
		{
			name:       "put hosted with proxy block",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badhosted",
			rawBody:    `{"proxy":{"upstream":"https://pypi.org"}}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid proxy: proxy block must be empty on mode \"hosted\""},
		},
		{
			name:       "put malformed json",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/badjson",
			rawBody:    `}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid JSON body: invalid character '}' looking for beginning of value"},
		},
		{
			name:       "put unknown json field",
			method:     http.MethodPut,
			path:       "/admin/v1/namespaces/unknownfield",
			rawBody:    `{"bogus":true}`,
			wantStatus: http.StatusBadRequest,
			wantBody:   map[string]string{"error": "invalid JSON body: json: unknown field \"bogus\""},
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
			name:       "list namespaces empty",
			method:     http.MethodGet,
			path:       "/admin/v1/namespaces",
			wantStatus: http.StatusOK,
			wantBody:   map[string][]string{"namespaces": {}},
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
			wantBody:   map[string][]string{"namespaces": {"alpha", "beta"}},
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
			if tc.rawBody != "" {
				body = []byte(tc.rawBody)
			} else if tc.body != nil {
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

// TestHandler_PutStampsSchemaVersionOnDisk proves the admin write
// path calls Spec.Normalize even when the request body omits
// schema_version: the persisted body must carry the current version
// so future ocifactory binaries can distinguish shapes without
// sniffing.
func TestHandler_PutStampsSchemaVersionOnDisk(t *testing.T) {
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

	body := bytes.NewReader([]byte(`{"policy":{"readers":[{"email":"alice@example.com"}]}}`))
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPut, srv.URL+"/admin/v1/namespaces/alpha", body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	stored, err := store.Get(t.Context(), "alpha")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Spec.SchemaVersion != namespace.CurrentSchemaVersion {
		t.Errorf("persisted SchemaVersion = %d, want %d", stored.Spec.SchemaVersion, namespace.CurrentSchemaVersion)
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
