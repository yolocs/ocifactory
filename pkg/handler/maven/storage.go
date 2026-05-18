package maven

import "strings"

const packageOwningRepoPrefix = "packages/"

func packageOwningRepo(repoParts string) string {
	return packageOwningRepoPrefix + repoParts
}

func repoPartsFromOwningRepo(owningRepo string) string {
	return strings.TrimPrefix(owningRepo, packageOwningRepoPrefix)
}
