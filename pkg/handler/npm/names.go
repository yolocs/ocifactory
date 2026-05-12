package npm

import (
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"
)

var npmNamePartRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9._~-]*$`)

func validatePackageName(name string) error {
	if name == "" {
		return fmt.Errorf("package name must not be empty")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("invalid package name %q", name)
	}
	if path.Clean(name) != name || name == "." || name == ".." || strings.Contains(name, "/../") {
		return fmt.Errorf("invalid package name %q", name)
	}
	parts := strings.Split(name, "/")
	if strings.HasPrefix(name, "@") {
		if len(parts) != 2 || len(parts[0]) < 2 {
			return fmt.Errorf("invalid scoped package name %q", name)
		}
		if !npmNamePartRegexp.MatchString(strings.TrimPrefix(parts[0], "@")) || !npmNamePartRegexp.MatchString(parts[1]) {
			return fmt.Errorf("invalid scoped package name %q", name)
		}
		return nil
	}
	if len(parts) != 1 || !npmNamePartRegexp.MatchString(name) {
		return fmt.Errorf("invalid package name %q", name)
	}
	return nil
}

func encodePackageName(npmName string) (string, error) {
	if err := validatePackageName(npmName); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Grow(len(npmName))
	for i := 0; i < len(npmName); i++ {
		c := npmName[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String(), nil
}

func decodePackageName(encodedPath string) (string, error) {
	var b strings.Builder
	b.Grow(len(encodedPath))
	for i := 0; i < len(encodedPath); i++ {
		c := encodedPath[i]
		if c != '_' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(encodedPath) {
			return "", fmt.Errorf("malformed escape at %d: trailing underscore", i)
		}
		decoded, err := hex.DecodeString(encodedPath[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("malformed escape at %d: %w", i, err)
		}
		b.WriteByte(decoded[0])
		i += 2
	}
	name := b.String()
	if err := validatePackageName(name); err != nil {
		return "", err
	}
	return name, nil
}

func packageBase(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}
