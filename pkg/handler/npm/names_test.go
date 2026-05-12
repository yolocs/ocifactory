package npm

import (
	"errors"
	"strings"
	"testing"
)

func TestValidatePackageName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "simple", input: "foo", wantErr: false},
		{name: "with dash", input: "foo-bar", wantErr: false},
		{name: "with dot", input: "socket.io", wantErr: false},
		{name: "with underscore", input: "foo_bar", wantErr: false},
		{name: "digits", input: "lodash4", wantErr: false},
		{name: "scoped", input: "@scope/foo", wantErr: false},
		{name: "scoped with hyphens", input: "@my-scope/some-pkg", wantErr: false},

		{name: "empty", input: "", wantErr: true},
		{name: "uppercase", input: "FooBar", wantErr: true},
		{name: "leading dot", input: ".foo", wantErr: true},
		{name: "leading underscore", input: "_foo", wantErr: true},
		{name: "leading dash", input: "-foo", wantErr: true},
		{name: "with slash unscoped", input: "foo/bar", wantErr: true},
		{name: "tilde", input: "foo~bar", wantErr: true},
		{name: "space", input: "foo bar", wantErr: true},
		{name: "scope without slash", input: "@scope", wantErr: true},
		{name: "scope empty", input: "@/foo", wantErr: true},
		{name: "scope name empty", input: "@scope/", wantErr: true},
		{name: "scope leading dot", input: "@.scope/foo", wantErr: true},
		{name: "scope uppercase", input: "@Scope/foo", wantErr: true},
		{name: "too long", input: strings.Repeat("a", maxPackageNameLength+1), wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validatePackageName(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validatePackageName(%q) err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr && err != nil && !errors.Is(err, ErrInvalidPackageName) {
				t.Errorf("validatePackageName(%q) returned err=%v, want errors.Is(err, ErrInvalidPackageName)", tc.input, err)
			}
		})
	}
}

func TestPackageOwningRepo(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "unscoped", input: "foo", want: "packages/u/foo"},
		{name: "unscoped with dash", input: "foo-bar", want: "packages/u/foo-bar"},
		{name: "scoped", input: "@scope/foo", want: "packages/s/scope/foo"},
		{name: "scoped with dash", input: "@my-scope/some-pkg", want: "packages/s/my-scope/some-pkg"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := packageOwningRepo(tc.input)
			if got != tc.want {
				t.Errorf("packageOwningRepo(%q) = %q, want %q", tc.input, got, tc.want)
			}
			back, err := parsePackageOwningRepo(got)
			if err != nil {
				t.Errorf("parsePackageOwningRepo(%q) err=%v", got, err)
			}
			if back != tc.input {
				t.Errorf("round-trip: parsePackageOwningRepo(packageOwningRepo(%q)) = %q", tc.input, back)
			}
		})
	}
}

func TestParsePackageOwningRepo_Malformed(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"packages",
		"packages/u",
		"packages/u/",
		"packages/s/scope",
		"packages/s/scope/",
		"packages/x/foo",
		"packages/u/foo/bar",
	}
	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			if _, err := parsePackageOwningRepo(tc); err == nil {
				t.Errorf("parsePackageOwningRepo(%q) returned nil error, want error", tc)
			}
		})
	}
}

func TestEncodePackageNameTag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "unscoped", input: "foo", want: "foo"},
		{name: "with dot", input: "socket.io", want: "socket.io"},
		{name: "with dash", input: "foo-bar", want: "foo-bar"},
		{name: "with underscore", input: "foo_bar", want: "foo_5Fbar"},
		{name: "scoped", input: "@scope/foo", want: "_40scope_2Ffoo"},
		{name: "scoped with underscore", input: "@scope/foo_bar", want: "_40scope_2Ffoo_5Fbar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := encodePackageNameTag(tc.input)
			if (err != nil) != tc.wantErr {
				t.Fatalf("encodePackageNameTag(%q) err=%v, wantErr=%v", tc.input, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if got != tc.want {
				t.Errorf("encodePackageNameTag(%q) = %q, want %q", tc.input, got, tc.want)
			}
			back, err := decodePackageNameTag(got)
			if err != nil {
				t.Errorf("decodePackageNameTag(%q) err=%v", got, err)
			}
			if back != tc.input {
				t.Errorf("round-trip: decodePackageNameTag(encodePackageNameTag(%q)) = %q", tc.input, back)
			}
		})
	}
}

func TestTarballFilename(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, pkg, version, want string
	}{
		{name: "unscoped", pkg: "foo", version: "1.0.0", want: "foo-1.0.0.tgz"},
		{name: "scoped strips @scope", pkg: "@scope/foo", version: "1.2.3", want: "foo-1.2.3.tgz"},
		{name: "scoped with dash", pkg: "@my-scope/some-pkg", version: "0.0.1", want: "some-pkg-0.0.1.tgz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tarballFilename(tc.pkg, tc.version)
			if got != tc.want {
				t.Errorf("tarballFilename(%q, %q) = %q, want %q", tc.pkg, tc.version, got, tc.want)
			}
		})
	}
}
