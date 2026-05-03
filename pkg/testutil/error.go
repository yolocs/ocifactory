// Package testutil holds small assertion helpers shared by tests across
// the ocifactory packages.
package testutil

import (
	"fmt"
	"strings"
)

// DiffErrString returns "" when got's message contains want; otherwise it
// returns a human-readable string describing the mismatch. If want is "",
// got must be nil.
//
// Intended use:
//
//	if diff := testutil.DiffErrString(err, "not found"); diff != "" {
//	    t.Errorf("ReadFile() error: %s", diff)
//	}
func DiffErrString(got error, want string) string {
	switch {
	case want == "" && got == nil:
		return ""
	case want == "" && got != nil:
		return fmt.Sprintf("got error %q, want <nil>", got.Error())
	case got == nil:
		return fmt.Sprintf("got <nil> error, want one containing %q", want)
	case !strings.Contains(got.Error(), want):
		return fmt.Sprintf("got error %q, want one containing %q", got.Error(), want)
	}
	return ""
}
