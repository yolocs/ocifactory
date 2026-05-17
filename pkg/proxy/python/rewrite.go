package python

import (
	"bytes"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/yolocs/ocifactory/pkg/proxy"
	"golang.org/x/net/html"
)

// RewriteSimpleIndex parses an upstream PEP 503 simple-index HTML body
// for one package and rewrites every <a href="..."> file URL to
// "/{namespace}/packages/{pkg}/{version}/{filename}#sha256=...".
//
// pkg is the PEP 503 normalized package name as the upstream knows it
// (used to extract version segments from filenames). namespace is the
// ocifactory namespace the request landed in.
//
// The fragment (`#sha256=...`) is preserved if upstream included one,
// since pip checks it for integrity. The data-* attributes
// (`data-requires-python`, `data-dist-info-metadata`,
// `data-core-metadata`, `data-yanked`) are passed through unchanged.
//
// Anchors that don't carry a parseable distribution filename are
// preserved as-is rather than dropped: the simple index occasionally
// carries non-file links (e.g. PEP 658 metadata sidecars share the
// same anchor shape and parse fine, but the goal is to fail soft if
// upstream adds a future shape we don't recognise yet).
//
// On a fundamentally unparseable body (HTML parser returns an error,
// which html.Parse essentially never does) the call surfaces
// [proxy.ErrUpstreamMalformed].
func RewriteSimpleIndex(body []byte, namespace, pkg string) ([]byte, error) {
	if namespace == "" || pkg == "" {
		return nil, fmt.Errorf("python proxy: rewrite: namespace and pkg are required")
	}
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("python proxy: parse simple index: %v: %w", err, proxy.ErrUpstreamMalformed)
	}
	rewriteAnchors(doc, namespace, pkg)
	var buf bytes.Buffer
	if err := html.Render(&buf, doc); err != nil {
		return nil, fmt.Errorf("python proxy: render simple index: %v: %w", err, proxy.ErrUpstreamMalformed)
	}
	return buf.Bytes(), nil
}

func rewriteAnchors(n *html.Node, namespace, pkg string) {
	if n.Type == html.ElementNode && n.Data == "a" {
		for i, attr := range n.Attr {
			if attr.Key != "href" {
				continue
			}
			rewritten, ok := rewriteHref(attr.Val, namespace, pkg)
			if ok {
				n.Attr[i].Val = rewritten
			}
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rewriteAnchors(c, namespace, pkg)
	}
}

// rewriteHref returns the namespace-prefixed URL for a single anchor
// href and reports whether the rewrite succeeded. When the upstream
// href doesn't look like a distribution download (no extractable
// filename or no parseable version), the original value is returned
// with ok=false so the caller leaves the attribute untouched — a
// malformed entry has no good replacement and dropping it would hide
// the upstream problem from the client.
func rewriteHref(href, namespace, pkg string) (string, bool) {
	if href == "" {
		return href, false
	}
	parsed, err := url.Parse(href)
	if err != nil {
		return href, false
	}
	filename := path.Base(parsed.Path)
	if filename == "" || filename == "." || filename == "/" {
		return href, false
	}
	version, err := parseFilenameVersion(filename, pkg)
	if err != nil {
		return href, false
	}
	// Construct a relative URL so the rewrite plays nicely behind
	// any reverse proxy / hostname the client is talking to. pip
	// resolves the relative path against the index URL it requested.
	rewritten := url.URL{Path: path.Join("/", namespace, "packages", pkg, version, filename)}
	if parsed.Fragment != "" {
		rewritten.Fragment = parsed.Fragment
	}
	if parsed.RawQuery != "" {
		// PyPI doesn't put query params on file URLs today, but
		// future hash-in-query schemes (the long-discussed PEP)
		// might. Pass through so the client gets to see them.
		rewritten.RawQuery = parsed.RawQuery
	}
	return rewritten.String(), true
}

// parseFilenameVersion extracts the version segment from a PyPI
// distribution filename, given the package's PEP 503 normalized name.
// Wheels follow PEP 427:
//
//	{distribution}-{version}(-{build tag})?-{python tag}-{abi tag}-{platform tag}.whl
//
// Sdists follow no formal PEP but conventionally:
//
//	{distribution}-{version}.{tar.gz | zip | tar.bz2}
//
// PEP 658 metadata sidecars append `.metadata` to the wheel filename.
// PyPI also accepts an older `.egg` shape we tolerate for completeness.
//
// The distribution segment may use either `-` or `_` in place of `-`
// in the canonical name (PyPI's filename convention). We match both
// candidates; the comparison is case-insensitive because PEP 503
// normalization lower-cases the canonical name but historical
// filenames preserve original casing.
func parseFilenameVersion(filename, normalizedPkg string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("empty filename")
	}
	if normalizedPkg == "" {
		return "", fmt.Errorf("empty package name")
	}
	lowerFilename := strings.ToLower(filename)
	candidates := []string{normalizedPkg}
	if strings.Contains(normalizedPkg, "-") {
		candidates = append(candidates, strings.ReplaceAll(normalizedPkg, "-", "_"))
	}

	for _, c := range candidates {
		prefix := strings.ToLower(c) + "-"
		if !strings.HasPrefix(lowerFilename, prefix) {
			continue
		}
		rest := filename[len(prefix):]

		switch {
		case strings.HasSuffix(lowerFilename, ".whl.metadata"):
			stem := rest[:len(rest)-len(".whl.metadata")]
			return wheelVersion(stem)
		case strings.HasSuffix(lowerFilename, ".whl"):
			stem := rest[:len(rest)-len(".whl")]
			return wheelVersion(stem)
		case strings.HasSuffix(lowerFilename, ".tar.gz"):
			return rest[:len(rest)-len(".tar.gz")], nil
		case strings.HasSuffix(lowerFilename, ".tar.bz2"):
			return rest[:len(rest)-len(".tar.bz2")], nil
		case strings.HasSuffix(lowerFilename, ".zip"):
			return rest[:len(rest)-len(".zip")], nil
		case strings.HasSuffix(lowerFilename, ".egg"):
			return rest[:len(rest)-len(".egg")], nil
		}
	}
	return "", fmt.Errorf("filename %q does not match package %q", filename, normalizedPkg)
}

// wheelVersion pulls the version segment from a wheel stem
// (`<version>-<py>-<abi>-<plat>` or
// `<version>-<build>-<py>-<abi>-<plat>`). PEP 427 specifies at least
// three trailing segments (py/abi/platform); a build tag adds a fourth.
// We split and pull the segment before the last three.
func wheelVersion(stem string) (string, error) {
	parts := strings.Split(stem, "-")
	if len(parts) < 4 {
		return "", fmt.Errorf("wheel stem %q has too few segments", stem)
	}
	// version is everything before the last three segments
	// (py-abi-platform). Build tag (if present) lives between
	// version and python-tag and is rolled into the version-slot
	// when len(parts) == 5, which would mis-attribute it; PEP 427
	// disambiguates by requiring the build tag to start with a
	// digit, but practically wheels with build tags are rare and we
	// don't need to support them to route pip — we'd just store
	// under a slightly wider version key. Keep this simple.
	versionSegments := parts[:len(parts)-3]
	return strings.Join(versionSegments, "-"), nil
}
