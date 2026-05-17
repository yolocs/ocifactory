package npm

import (
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"

	"github.com/yolocs/ocifactory/pkg/proxy"
)

// RewritePackument rewrites every versions[*].dist.tarball URL in an
// upstream npm packument so npm clients route tarball downloads back
// through the ocifactory namespace they queried.
func RewritePackument(body []byte, namespace string) ([]byte, error) {
	if namespace == "" {
		return nil, fmt.Errorf("npm proxy: rewrite: namespace is required")
	}
	meta, err := decodePackument(body, "")
	if err != nil {
		return nil, err
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("npm proxy: rewrite packument: %v: %w", err, proxy.ErrUpstreamMalformed)
	}
	versions, ok := doc["versions"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("npm proxy: rewrite packument: versions is not an object: %w", proxy.ErrUpstreamMalformed)
	}

	for version, rawVersion := range versions {
		versionDoc, ok := rawVersion.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("npm proxy: rewrite packument: version %q is not an object: %w", version, proxy.ErrUpstreamMalformed)
		}
		dist, ok := versionDoc["dist"].(map[string]any)
		if !ok {
			continue
		}
		rawTarball, ok := dist["tarball"].(string)
		if !ok || rawTarball == "" {
			continue
		}
		parsed, err := url.Parse(rawTarball)
		if err != nil {
			return nil, fmt.Errorf("npm proxy: rewrite packument: version %q tarball %q: %v: %w",
				version, rawTarball, err, proxy.ErrUpstreamMalformed)
		}
		filename := path.Base(parsed.Path)
		if filename == "" || filename == "." || filename == "/" {
			continue
		}
		pkg := meta.Name
		if vName, ok := versionDoc["name"].(string); ok && vName != "" {
			pkg = vName
		}
		dist["tarball"] = namespaceTarballPath(namespace, pkg, filename)
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("npm proxy: rewrite packument: marshal: %w", err)
	}
	return out, nil
}

func namespaceTarballPath(namespace, pkg, filename string) string {
	segments := []string{"", url.PathEscape(namespace)}
	if strings.HasPrefix(pkg, "@") {
		scope, name, ok := strings.Cut(pkg, "/")
		if ok {
			segments = append(segments, url.PathEscape(scope), url.PathEscape(name))
		} else {
			segments = append(segments, url.PathEscape(pkg))
		}
	} else {
		segments = append(segments, url.PathEscape(pkg))
	}
	segments = append(segments, "-", url.PathEscape(filename))
	return strings.Join(segments, "/")
}
