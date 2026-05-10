package namespace

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		in      string
		wantErr bool
	}{
		// Allowed shapes.
		{"single-letter", "a", false},
		{"single-digit", "0", false},
		{"simple", "myteam", false},
		{"hyphenated", "my-team", false},
		{"multiple-hyphens", "my-team-v2alpha", false},
		{"with-numbers", "team42", false},
		{"max-length", strings.Repeat("a", 64), false},

		// Length bounds.
		{"empty", "", true},
		{"too-long", strings.Repeat("a", 65), true},

		// Hyphen position.
		{"leading-hyphen", "-team", true},
		{"trailing-hyphen", "team-", true},
		{"only-hyphen", "-", true},

		// Disallowed character classes.
		{"uppercase", "MyTeam", true},
		{"contains-dot", "my.team", true},
		{"contains-slash", "my/team", true},
		{"contains-underscore", "my_team", true},
		{"contains-space", "my team", true},
		{"non-ascii", "tëam", true},

		// Reserved prefixes.
		{"underscore-prefix", "_internal", true},
		{"dot-prefix", ".hidden", true},

		// Reserved names.
		{"reserved-admin", "admin", true},
		{"reserved-healthz", "healthz", true},
		{"reserved-readyz", "readyz", true},
		{"reserved-metrics", "metrics", true},
		{"reserved-simple", "simple", true},
		{"reserved-maven2", "maven2", true},
		{"reserved-v2", "v2", true},
		{"reserved-npm", "npm", true},
		{"reserved-_meta", "_meta", true},
		{"reserved-_catalog", "_catalog", true},
		{"reserved-_index", "_index", true},
		{"reserved-_namespaces", "_namespaces", true},
		{"reserved-_packages", "_packages", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ValidateName(%q) = nil, want error", tc.in)
				}
				if !errors.Is(err, ErrInvalidName) {
					t.Errorf("ValidateName(%q) error %v, want errors.Is ErrInvalidName", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateName(%q) = %v, want nil", tc.in, err)
			}
		})
	}
}
