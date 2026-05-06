package python

import "testing"

// TestNormalize covers the PEP 503 algorithm: lowercase + collapse runs
// of `.`, `-`, `_` to a single `-`. Idempotence, mixed-separator runs,
// and the normalize-to-equal cases that motivated this work all live
// here.
func TestNormalize(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "already normalized lowercase", in: "requests", want: "requests"},
		{name: "uppercase", in: "Requests", want: "requests"},
		{name: "underscore separator", in: "Foo_Bar", want: "foo-bar"},
		{name: "dash separator", in: "Foo-Bar", want: "foo-bar"},
		{name: "dot separator", in: "Foo.Bar", want: "foo-bar"},
		{name: "mixed separators run", in: "Foo._-_Bar", want: "foo-bar"},
		{name: "all caps with dots", in: "ZOPE.INTERFACE", want: "zope-interface"},
		{name: "trailing separators preserved as single dash", in: "foo___", want: "foo-"},
		{name: "leading separators preserved as single dash", in: "___foo", want: "-foo"},
		{name: "digits and dashes", in: "pkg-2-foo", want: "pkg-2-foo"},
		{name: "idempotent", in: "foo-bar", want: "foo-bar"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalize(tc.in); got != tc.want {
				t.Errorf("normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalize_Idempotence(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"requests", "Foo_Bar", "ZOPE.INTERFACE", "Foo._-_Bar",
		"pkg-2-foo", "Pkg.With.Dots", "MIXED__case",
	}
	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			once := normalize(in)
			twice := normalize(once)
			if once != twice {
				t.Errorf("normalize is not idempotent: normalize(%q)=%q; normalize(normalize(%q))=%q", in, once, in, twice)
			}
		})
	}
}
