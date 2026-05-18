package npm

import "strings"

const (
	packageIndexName        = "npm-packages"
	packageOwningRepoPrefix = "packages"
)

// packageOwningRepo returns the OCI [oci.RepoFile.OwningRepo] string
// for an npm package. Callers must already have validated name via
// [validatePackageName]; this function does NOT re-validate.
//
//   - unscoped "foo"          -> "packages/u/foo"
//   - scoped   "@scope/foo"   -> "packages/s/scope/foo"
func packageOwningRepo(name string) string {
	if strings.HasPrefix(name, "@") {
		rest := name[1:]
		slash := strings.IndexByte(rest, '/')
		scope, unscoped := rest[:slash], rest[slash+1:]
		return packageOwningRepoPrefix + "/s/" + scope + "/" + unscoped
	}
	return packageOwningRepoPrefix + "/u/" + name
}
