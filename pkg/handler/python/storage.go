package python

import "strings"

const packageIndexName = "python-packages"

const packageOwningRepoPrefix = "packages/"

func packageOwningRepo(name string) string {
	return packageOwningRepoPrefix + normalize(name)
}

func packageFromOwningRepo(repo string) (string, bool) {
	pkg, ok := strings.CutPrefix(repo, packageOwningRepoPrefix)
	return pkg, ok && pkg != ""
}
