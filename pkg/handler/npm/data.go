package npm

// Placeholder for package metadata
type PackageMetadata struct {
	Name           string                    `json:"name"`
	Description    string                    `json:"description,omitempty"`
	DistTags       map[string]string         `json:"dist-tags"`
	Versions       map[string]VersionInfo    `json:"versions"`
	Time           map[string]string         `json:"time,omitempty"` // Creation and modification timestamps
	Maintainers    []Maintainer              `json:"maintainers,omitempty"`
	Readme         string                    `json:"readme,omitempty"`
	ReadmeFilename string                    `json:"readmeFilename,omitempty"`
	Homepage       string                    `json:"homepage,omitempty"`
	Keywords       []string                  `json:"keywords,omitempty"`
	Repository     *Repository               `json:"repository,omitempty"`
	Bugs           *Bugs                     `json:"bugs,omitempty"`
	License        any                       `json:"license,omitempty"`      // Can be string or object
	Attachments    map[string]AttachmentStub `json:"_attachments,omitempty"` // For publish
	Rev            string                    `json:"_rev,omitempty"`         // CouchDB revision
	ID             string                    `json:"_id,omitempty"`          // CouchDB ID
}

// Placeholder for version-specific package information
type VersionInfo struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	Description     string            `json:"description,omitempty"`
	Main            string            `json:"main,omitempty"`
	Scripts         map[string]string `json:"scripts,omitempty"`
	Dependencies    map[string]string `json:"dependencies,omitempty"`
	DevDependencies map[string]string `json:"devDependencies,omitempty"`
	Dist            Dist              `json:"dist"`
	Author          any               `json:"author,omitempty"` // Can be string or object
	Maintainers     []Maintainer      `json:"maintainers,omitempty"`
	Keywords        []string          `json:"keywords,omitempty"`
	License         any               `json:"license,omitempty"`
	Homepage        string            `json:"homepage,omitempty"`
	Repository      *Repository       `json:"repository,omitempty"`
	Bugs            *Bugs             `json:"bugs,omitempty"`
	GitHead         string            `json:"gitHead,omitempty"`
	NodeVersion     string            `json:"_nodeVersion,omitempty"`
	NpmVersion      string            `json:"_npmVersion,omitempty"`
	NpmUser         *User             `json:"_npmUser,omitempty"`
	ID              string            `json:"_id,omitempty"`     // Usually name@version
	Shasum          string            `json:"_shasum,omitempty"` // Alias for dist.shasum
	From            string            `json:"_from,omitempty"`   // For dependencies
	Tarball         string            `json:"tarball,omitempty"` // For internal use, deprecated
}

// Placeholder for distribution files (tarball)
type Dist struct {
	Shasum       string `json:"shasum"`
	Tarball      string `json:"tarball"`             // URL to the tarball
	Integrity    string `json:"integrity,omitempty"` // Subresource Integrity string
	FileCount    int    `json:"fileCount,omitempty"`
	UnpackedSize int    `json:"unpackedSize,omitempty"`
	NpmSignature string `json:"npm-signature,omitempty"` // Signature of the tarball
}

// Placeholder for maintainer information
type Maintainer struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// Placeholder for repository information
type Repository struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Directory string `json:"directory,omitempty"`
}

// Placeholder for bug tracking information
type Bugs struct {
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

// Placeholder for user information
type User struct {
	Name string `json:"name"`
}

// Placeholder for attachment stub (used in publish)
type AttachmentStub struct {
	ContentType string `json:"content_type"`
	Data        string `json:"data"` // Base64 encoded tarball data
	Length      int    `json:"length"`
}

// Response for successful publish/unpublish
type ModifyResponse struct {
	Ok      bool   `json:"ok"`
	ID      string `json:"id,omitempty"`
	Rev     string `json:"rev,omitempty"`
	Success bool   `json:"success,omitempty"` // Used by some clients
}

// Abbreviated Package Metadata for search and abbreviated GETs
type AbbreviatedPackageMetadata struct {
	Name        string            `json:"name"`
	Version     string            `json:"version,omitempty"` // Only for "application/vnd.npm.install-v1+json"
	Description string            `json:"description,omitempty"`
	DistTags    map[string]string `json:"dist-tags,omitempty"`
	Modified    string            `json:"modified,omitempty"`
	Versions    map[string]any    `json:"versions,omitempty"` // Can be full VersionInfo or just a subset
	Maintainers []Maintainer      `json:"maintainers,omitempty"`
	Time        map[string]string `json:"time,omitempty"`
	Homepage    string            `json:"homepage,omitempty"`
	Keywords    []string          `json:"keywords,omitempty"`
	Repository  *Repository       `json:"repository,omitempty"`
	Author      any               `json:"author,omitempty"`
	Bugs        *Bugs             `json:"bugs,omitempty"`
	License     any               `json:"license,omitempty"`
}

// Abbreviated Version Metadata for "application/vnd.npm.install-v1+json"
type AbbreviatedVersionInfo struct {
	Name                 string            `json:"name"`
	Version              string            `json:"version"`
	Description          string            `json:"description,omitempty"`
	Dependencies         map[string]string `json:"dependencies,omitempty"`
	OptionalDependencies map[string]string `json:"optionalDependencies,omitempty"`
	DevDependencies      map[string]string `json:"devDependencies,omitempty"`
	PeerDependencies     map[string]string `json:"peerDependencies,omitempty"`
	BundleDependencies   []string          `json:"bundleDependencies,omitempty"`
	Bin                  map[string]string `json:"bin,omitempty"`
	Directories          map[string]string `json:"directories,omitempty"`
	Dist                 Dist              `json:"dist"`
	Engines              map[string]string `json:"engines,omitempty"`
	Deprecated           string            `json:"deprecated,omitempty"`
	HasInstallScript     bool              `json:"hasInstallScript,omitempty"`
	ID                   string            `json:"_id,omitempty"` // name@version
	Shasum               string            `json:"_shasum"`
	Resolved             string            `json:"_resolved,omitempty"`
}
