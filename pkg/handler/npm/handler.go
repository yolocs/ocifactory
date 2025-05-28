package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json" // Already present, but good to note
	"fmt"           // Already present
	"io"            // Already present
	"net/http"      // Already present
	"path/filepath" // Already present
	"regexp"        // Already present
	"sort"          // Already present
	"strings"       // Already present
	"time"          // Already present

	"github.com/Masterminds/semver/v3"
	"github.com/gorilla/mux"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/yolocs/ocifactory/pkg/errors" // For structured errors
	"github.com/yolocs/ocifactory/pkg/handler"
	npmdata "github.com/yolocs/ocifactory/pkg/handler/npm/data"
	"github.com/yolocs/ocifactory/pkg/oci"
)

const (
	RepoType                  = "npm"
	ArtifactType              = "application/vnd.ocifactory.npm.versioninfo.v1+json" // For the version metadata JSON
	TarballArtifactType       = "application/vnd.npm.package.tar+gzip"               // Hypothetical media type for tarballs - used for clarity if we had more control
	DefaultTarballContentType = "application/gzip"                                   // Used for AddFile for .tgz
	VersionInfoFilename       = "package.json"                                       // Standard name for the version metadata file within the OCI "manifest"
)

// Regex to extract version from tarball filename like name-1.0.0.tgz or @scope/name-1.0.0.tgz
// It expects the version to be at the end, preceded by a hyphen.
var versionRegex = regexp.MustCompile(`(?:[^/]+/)?([^/]+?)-(\d+\.\d+\.\d+(?:-[^{}+]+(?:\.[^{}+]+)*)?(?:[+]{1}[^{}\s]+)?)\.tgz$`)

type Handler struct {
	registry handler.Registry
}

func NewHandler(registry handler.Registry) (*Handler, error) {
	return &Handler{registry: registry}, nil
}

func (h *Handler) Mux() http.Handler {
	r := mux.NewRouter()

	// Package Read APIs
	// GET /{package}/{versionOrTag} - Must be specific, order matters with mux
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/{versionOrTag}", getPackageVersionMetadataHandler).Methods(http.MethodGet, http.MethodHead)
	// GET /{package} - General package info (full metadata)
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", getPackageMetadataHandler).Methods(http.MethodGet, http.MethodHead)

	// Tarball Download
	// GET /@scope/package/-/package-version.tgz
	// GET /package/-/package-version.tgz
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}", downloadTarballHandler).Methods(http.MethodGet, http.MethodHead)

	// Package Write APIs (Publish, Unpublish)
	// PUT /@scope/package
	// PUT /package
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}", publishPackageHandler).Methods(http.MethodPut)

	// Unpublish specific version: DELETE /@scope/package/-/filename.tgz/-rev/revision
	// Unpublish specific version: DELETE /package/-/filename.tgz/-rev/revision
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-/{filename:.+\\.tgz}/-rev/{revision}", unpublishPackageHandler).Methods(http.MethodDelete)
	// Unpublish entire package: DELETE /@scope/package/-rev/revision
	// Unpublish entire package: DELETE /package/-rev/revision
	r.HandleFunc("/{package:(?:@[^/]+/)?[^/@][^/]*}/-rev/{revision}", unpublishPackageHandler).Methods(http.MethodDelete)

	// User Management & Authentication not supported.
	// PUT /-/user/org.couchdb.user:{username}
	// r.HandleFunc("/-/user/{username:org\\.couchdb\\.user:[^/]+}", UserLoginHandler).Methods(http.MethodPut)
	// GET /-/whoami or /-/npm/v1/user
	// r.HandleFunc("/-/whoami", WhoamiHandler).Methods(http.MethodGet, http.MethodHead)
	// r.HandleFunc("/-/npm/v1/user", WhoamiHandler).Methods(http.MethodGet, http.MethodHead) // Newer endpoint

	// Dist Tags (npm dist-tag add/rm/ls)
	// The npm client often modifies dist-tags by PUTting the whole package document.
	// However, a more direct API might look like this:
	// PUT /-/package/@scope/pkg/dist-tags/latest (body: "1.0.0")
	// These are examples and might vary based on exact npm client behavior with different registry versions.
	// A common way npm CLI handles this is to GET the package doc, modify dist-tags, then PUT the package doc.
	// The following are more explicit/granular endpoints if you want to implement them directly.
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", distTagAddHandler).Methods(http.MethodPut, http.MethodPost)
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags/{tag}", distTagRmHandler).Methods(http.MethodDelete)
	r.HandleFunc("/-/package/{package:(?:@[^/]+/)?[^/@][^/]*}/dist-tags", distTagLsHandler).Methods(http.MethodGet, http.MethodHead)

	// Ping.
	r.HandleFunc("/-/ping", pingHandler).Methods(http.MethodGet)
	r.HandleFunc("/", pingHandler).Methods(http.MethodGet)

	return r
}

func getPackageVersionMetadataHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgName := vars["package"]
	versionOrTag := vars["versionOrTag"]
	ctx := req.Context()

	ociRepoName := RepoType + "/" + strings.Replace(pkgName, "@", "", 1)

	desc, err := h.registry.Resolve(ctx, ociRepoName, versionOrTag)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("package version %s@%s not found: %v", pkgName, versionOrTag, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to resolve %s@%s: %v", pkgName, versionOrTag, err), http.StatusInternalServerError)
		}
		return
	}

	manifest, err := h.registry.GetManifest(ctx, ociRepoName, desc.Digest)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("manifest for %s@%s (digest %s) not found: %v", pkgName, versionOrTag, desc.Digest, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get manifest for %s@%s (digest %s): %v", pkgName, versionOrTag, desc.Digest, err), http.StatusInternalServerError)
		}
		return
	}

	// Find the VersionInfo JSON layer
	var versionInfoLayer ocispec.Descriptor
	foundVersionInfoLayer := false
	for _, layer := range manifest.Layers {
		if layer.MediaType == ArtifactType {
			// Primary way: Match by specific media type for VersionInfo
			versionInfoLayer = layer
			foundVersionInfoLayer = true
			break
		}
		// Fallback or alternative: check annotation for filename, e.g. "package.json"
		if title, ok := layer.Annotations[ocispec.AnnotationTitle]; ok && title == VersionInfoFilename {
			versionInfoLayer = layer
			foundVersionInfoLayer = true
			break
		}
	}

	if !foundVersionInfoLayer {
		http.Error(w, fmt.Sprintf("VersionInfo JSON layer not found in manifest for %s@%s", pkgName, versionOrTag), http.StatusInternalServerError)
		return
	}

	blob, err := h.registry.GetBlob(ctx, ociRepoName, versionInfoLayer.Digest)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("npm version info blob for %s@%s (digest %s) not found: %v", pkgName, versionOrTag, versionInfoLayer.Digest, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get npm version info blob for %s@%s (digest %s): %v", pkgName, versionOrTag, versionInfoLayer.Digest, err), http.StatusInternalServerError)
		}
		return
	}

	var versionInfo npmdata.VersionInfo
	if err := json.Unmarshal(blob, &versionInfo); err != nil {
		http.Error(w, fmt.Sprintf("failed to unmarshal npm version info for %s@%s: %v", pkgName, versionOrTag, err), http.StatusInternalServerError)
		return
	}

	// Ensure the version in the response matches the requested versionOrTag if it's a valid version string
	// (not 'latest' or other tags). The VersionInfo itself should contain the canonical version.
	// No major changes needed here as VersionInfo.Version is the source of truth from the blob.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(versionInfo); err != nil {
		// Log error, headers might have been sent
		fmt.Printf("Error encoding version metadata for %s@%s: %v\n", pkgName, versionOrTag, err)
	}
}

func getPackageMetadataHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgName := vars["package"]
	ctx := req.Context()

	// Construct OCI repository name
	// For scoped packages like @scope/pkg, the OCI repo might be npm/scope/pkg
	// For unscoped packages like pkg, it might be npm/pkg
	ociRepoName := RepoType + "/" + strings.Replace(pkgName, "@", "", 1)

	tags, err := h.registry.ListTags(ctx, ociRepoName)
	if err != nil {
		// TODO: Differentiate between "not found" and other errors from registry
		http.Error(w, fmt.Sprintf("failed to list tags for %s: %v", ociRepoName, err), http.StatusInternalServerError)
		return
	}

	if len(tags) == 0 {
		http.Error(w, fmt.Sprintf("package %s not found (no versions)", pkgName), http.StatusNotFound)
		return
	}

	versions := make(map[string]npmdata.VersionInfo)
	var versionCreationTimes []time.Time
	var latestSemVer *semver.Version
	latestTag := ""

	for _, tag := range tags {
		desc, err := h.registry.Resolve(ctx, ociRepoName, tag)
		if err != nil {
			// Log and continue, some tags might be problematic
			fmt.Printf("Error resolving tag %s for %s: %v\n", tag, ociRepoName, err)
			continue
		}

		manifest, err := h.registry.GetManifest(ctx, ociRepoName, desc.Digest)
		if err != nil {
			fmt.Printf("Error getting manifest for %s@%s (%s): %v\n", ociRepoName, tag, desc.Digest, err)
			continue
		}

		// Find the VersionInfo JSON layer within the manifest's layers
		var versionInfoLayer ocispec.Descriptor
		foundVersionInfoLayer := false
		for _, layer := range manifest.Layers {
			if layer.MediaType == ArtifactType {
				versionInfoLayer = layer
				foundVersionInfoLayer = true
				break
			}
			// Fallback: check annotation for filename, e.g. "package.json"
			if title, ok := layer.Annotations[ocispec.AnnotationTitle]; ok && title == VersionInfoFilename {
				versionInfoLayer = layer
				foundVersionInfoLayer = true
				break
			}
		}

		if !foundVersionInfoLayer {
			fmt.Printf("VersionInfo JSON layer not found in manifest for %s@%s. Skipping this version.\n", ociRepoName, tag)
			continue
		}
		
		blob, err := h.registry.GetBlob(ctx, ociRepoName, versionInfoLayer.Digest)
		if err != nil {
			fmt.Printf("Error getting blob for npm version info %s@%s (digest %s): %v\n", ociRepoName, tag, versionInfoLayer.Digest, err)
			continue
		}

		var versionInfo npmdata.VersionInfo
		if err := json.Unmarshal(blob, &versionInfo); err != nil {
			fmt.Printf("Error unmarshalling npm version info for %s@%s: %v\n", ociRepoName, tag, err)
			continue
		}

		// Ensure version string in VersionInfo matches the tag if necessary, or use tag as the key.
		// The version from the JSON (`versionInfo.Version`) should ideally match the `tag`.
		if versionInfo.Version == "" {
			versionInfo.Version = tag // Fallback if not in JSON
		}
		versions[versionInfo.Version] = versionInfo

		// Simplified time tracking: use current time as placeholder for creation/modification
		// A real implementation would get this from OCI artifact properties if available
		// or from the VersionInfo if it stores timestamps.
		// For now, just to populate the Time field.
		t := time.Now().UTC() // Placeholder
		versionCreationTimes = append(versionCreationTimes, t)

		// Determine latest tag using semantic versioning
		sv, err := semver.NewVersion(tag)
		if err == nil {
			if latestSemVer == nil || sv.GreaterThan(latestSemVer) {
				latestSemVer = sv
				latestTag = tag
			}
		}
	}

	if len(versions) == 0 {
		http.Error(w, fmt.Sprintf("no processable versions found for package %s", pkgName), http.StatusNotFound)
		return
	}

	// Populate PackageMetadata
	metadata := npmdata.PackageMetadata{
		Name:     pkgName,
		Versions: versions,
		DistTags: make(map[string]string),
	}

	if latestTag != "" {
		metadata.DistTags["latest"] = latestTag
		// Populate top-level fields from the latest version
		if latestVersionInfo, ok := versions[latestTag]; ok {
			metadata.Description = latestVersionInfo.Description
			metadata.Maintainers = latestVersionInfo.Maintainers
			metadata.Homepage = latestVersionInfo.Homepage
			metadata.Keywords = latestVersionInfo.Keywords
			metadata.Repository = latestVersionInfo.Repository
			metadata.Bugs = latestVersionInfo.Bugs
			metadata.License = latestVersionInfo.License
			// metadata.ID and metadata.Rev are CouchDB specific, may not be directly applicable
			metadata.ID = pkgName
		}
	}

	// Populate Time map (simplified)
	metadata.Time = make(map[string]string)
	if len(versionCreationTimes) > 0 {
		sort.Slice(versionCreationTimes, func(i, j int) bool { return versionCreationTimes[i].Before(versionCreationTimes[j]) })
		metadata.Time["created"] = versionCreationTimes[0].Format(time.RFC3339)
		metadata.Time["modified"] = versionCreationTimes[len(versionCreationTimes)-1].Format(time.RFC3339)
		// Add individual version timestamps
		for vTag, vInfo := range versions {
			// If VersionInfo had its own timestamp, use that. Otherwise, use a placeholder or skip.
			// For this example, using the "modified" time for all versions for simplicity.
			metadata.Time[vTag] = versionCreationTimes[len(versionCreationTimes)-1].Format(time.RFC3339)
		}
	}
	// Add a "latest" timestamp if not already covered by a specific version tag in Time map
	if latestTag != "" && metadata.Time[latestTag] == "" {
		 metadata.Time[latestTag] = versionCreationTimes[len(versionCreationTimes)-1].Format(time.RFC3339)
	}


	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(metadata); err != nil {
		// Log error, but headers might have already been sent
		fmt.Printf("Error encoding package metadata for %s: %v\n", pkgName, err)
	}
}

func downloadTarballHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"] // This can be @scope/name or just name
	filename := vars["filename"]      // This is usually name-version.tgz or just version.tgz for scoped pkgs
	ctx := req.Context()

	// Extract package name and version from filename and path
	// Example: /@scope/pkg/-/pkg-1.2.3.tgz -> pkgNameFromURL: @scope/pkg, filename: pkg-1.2.3.tgz. Version should be 1.2.3
	// Example: /pkg/-/pkg-1.2.3.tgz -> pkgNameFromURL: pkg, filename: pkg-1.2.3.tgz. Version should be 1.2.3
	
	parsedVersion := ""
	matches := versionRegex.FindStringSubmatch(filename)
	if len(matches) == 3 {
		// matches[1] is the package name part from filename, matches[2] is the version
		// We should ensure matches[1] is consistent with pkgNameFromURL if needed,
		// especially for unscoped packages. For scoped, pkgNameFromURL already has the full scope.
		parsedVersion = matches[2]
	}

	if parsedVersion == "" {
		// Fallback or simple extraction if regex fails: attempt to strip .tgz and split by last hyphen
		nameAndVersion := strings.TrimSuffix(filename, ".tgz")
		if lastHyphen := strings.LastIndex(nameAndVersion, "-"); lastHyphen != -1 && lastHyphen < len(nameAndVersion)-1 {
			parsedVersion = nameAndVersion[lastHyphen+1:]
		} else {
			http.Error(w, fmt.Sprintf("could not parse version from filename: %s", filename), http.StatusBadRequest)
			return
		}
	}
	
	// Construct OCI repository name (e.g., npm/scope/pkg or npm/pkg)
	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	// Resolve the tag (version) to get a manifest descriptor
	desc, err := h.registry.Resolve(ctx, ociRepoName, parsedVersion)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("package version %s@%s not found for tarball: %v", pkgNameFromURL, parsedVersion, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to resolve %s@%s for tarball: %v", pkgNameFromURL, parsedVersion, err), http.StatusInternalServerError)
		}
		return
	}

	// Fetch the manifest
	manifest, err := h.registry.GetManifest(ctx, ociRepoName, desc.Digest)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("manifest for %s@%s (digest %s) not found for tarball: %v", pkgNameFromURL, parsedVersion, desc.Digest, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get manifest for %s@%s (digest %s) for tarball: %v", pkgNameFromURL, parsedVersion, desc.Digest, err), http.StatusInternalServerError)
		}
		return
	}

	// Identify the tarball layer.
	var tarballLayerDesc ocispec.Descriptor
	foundTarballLayer := false
	for _, layer := range manifest.Layers {
		if layer.MediaType == TarballArtifactType {
			// Primary way: Match by specific media type for tarballs
			tarballLayerDesc = layer
			foundTarballLayer = true
			break
		}
		// Fallback or alternative: check annotation for the exact filename.
		// The `filename` var already contains the expected tarball filename (e.g. mypkg-1.0.0.tgz)
		if title, ok := layer.Annotations[ocispec.AnnotationTitle]; ok && title == filename {
			tarballLayerDesc = layer
			foundTarballLayer = true
			break
		}
	}

	if !foundTarballLayer {
		http.Error(w, fmt.Sprintf("tarball layer not found in manifest for %s@%s (filename: %s)", pkgNameFromURL, parsedVersion, filename), http.StatusInternalServerError)
		return
	}
	
	// Fetch the tarball blob
	blobReader, err := h.registry.GetBlob(ctx, ociRepoName, tarballLayerDesc.Digest)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("tarball blob %s for %s@%s not found: %v", tarballLayerDesc.Digest, pkgNameFromURL, parsedVersion, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("failed to get tarball blob %s for %s@%s: %v", tarballLayerDesc.Digest, pkgNameFromURL, parsedVersion, err), http.StatusInternalServerError)
		}
		return
	}
	defer blobReader.Close()

	// Set headers and stream response
	w.Header().Set("Content-Type", DefaultTarballContentType) // Or tarballLayerDesc.MediaType if it's specific and accurate
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(filename))) // Use filepath.Base for security
	w.Header().Set("Content-Length", fmt.Sprintf("%d", tarballLayerDesc.Size)) // Set Content-Length if available
	
	if _, err := io.Copy(w, blobReader); err != nil {
		// Hard to send a different status code if headers already sent. Log the error.
		fmt.Printf("Error streaming tarball for %s@%s: %v\n", pkgNameFromURL, parsedVersion, err)
	}
}

func publishPackageHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"]
	ctx := req.Context()

	// 1. Parse Request Body
	var pkgMeta npmdata.PackageMetadata
	if err := json.NewDecoder(req.Body).Decode(&pkgMeta); err != nil {
		http.Error(w, fmt.Sprintf("failed to parse request body: %v", err), http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	// Ensure package name from URL matches ID in body, if _id is present.
	// The _id field is usually `name` for npm, but can be different in CouchDB.
	// For simplicity, we'll use pkgNameFromURL as the canonical name.
	if pkgMeta.ID != "" && pkgMeta.ID != pkgNameFromURL {
		// Allowing this, but pkgNameFromURL is the OCI repo basis.
		fmt.Printf("Warning: Package name from URL (%s) differs from _id in body (%s)\n", pkgNameFromURL, pkgMeta.ID)
	}
	if pkgMeta.Name != "" && pkgMeta.Name != pkgNameFromURL {
		fmt.Printf("Warning: Package name from URL (%s) differs from name in body (%s)\n", pkgNameFromURL, pkgMeta.Name)
	}


	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	// 2. Iterate through _attachments
	if len(pkgMeta.Attachments) == 0 {
		// This might be a metadata-only update (e.g. changing dist-tags without new versions)
		// Or it might be an error if no versions are present either.
		// For now, we assume publish means new code, so attachments are expected.
		fmt.Printf("No attachments found for package %s. Processing dist-tags only.\n", pkgNameFromURL)
	}

	publishedVersions := make(map[string]npmdata.VersionInfo)

	for attachmentFilename, attachmentStub := range pkgMeta.Attachments {
		// Decode base64 tarball data
		tarballBytes, err := base64.StdEncoding.DecodeString(attachmentStub.Data)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to decode attachment %s: %v", attachmentFilename, err), http.StatusBadRequest)
			return
		}

		// Extract version from attachment filename
		// Filename in attachment key might be just "name-version.tgz" or "name-version.tgz"
		// The versionRegex expects [scope/]name-version.tgz, but attachments are simpler.
		// Let's try a simpler regex for attachment keys or direct string manipulation.
		var versionStr string
		matches := versionRegex.FindStringSubmatch(attachmentFilename) // versionRegex might be too complex here
		if len(matches) == 3 {
			versionStr = matches[2]
		} else {
			// Fallback: Try to extract from simpler "name-version.tgz"
			simpleVersionRegex := regexp.MustCompile(`^[^/]+?-(\d+\.\d+\.\d+(?:-[^{}+]+(?:\.[^{}+]+)*)?(?:[+]{1}[^{}\s]+)?)\.tgz$`)
			simpleMatches := simpleVersionRegex.FindStringSubmatch(attachmentFilename)
			if len(simpleMatches) == 2 {
				versionStr = simpleMatches[1]
			} else {
				fmt.Printf("Could not reliably extract version from attachment filename: %s. Skipping attachment.\n", attachmentFilename)
				continue
			}
		}


		// Find corresponding VersionInfo
		versionInfo, ok := pkgMeta.Versions[versionStr]
		if !ok {
			http.Error(w, fmt.Sprintf("VersionInfo for version %s (from attachment %s) not found in 'versions' map", versionStr, attachmentFilename), http.StatusBadRequest)
			return
		}

		// Validate shasum if present
		if versionInfo.Dist.Shasum != "" {
			h := sha256.New()
			h.Write(tarballBytes)
			calculatedShasum := fmt.Sprintf("%x", h.Sum(nil))
			if calculatedShasum != versionInfo.Dist.Shasum {
				http.Error(w, fmt.Sprintf("Shasum mismatch for %s: provided %s, calculated %s", attachmentFilename, versionInfo.Dist.Shasum, calculatedShasum), http.StatusBadRequest)
				return
			}
		}

		// Push Tarball
		tarballRepoFile := &oci.RepoFile{
			OwningRepo: ociRepoName,
			OwningTag:  versionStr, // Tag the manifest with the version string
			Name:       attachmentFilename,
			MediaType:  TarballArtifactType, // Use the more specific TarballArtifactType
		}
		_, err = h.registry.AddFile(ctx, tarballRepoFile, bytes.NewReader(tarballBytes))
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to push tarball for %s@%s: %v", pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}

		// Push VersionInfo JSON (as package.json)
		versionInfoJSONBytes, err := json.Marshal(versionInfo)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to marshal VersionInfo for %s@%s: %v", pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}
		versionInfoRepoFile := &oci.RepoFile{
			OwningRepo: ociRepoName,
			OwningTag:  versionStr, // Add to the same manifest tagged with versionStr
			Name:       VersionInfoFilename, 
			MediaType:  ArtifactType, // This is 'application/vnd.ocifactory.npm.versioninfo.v1+json'
		}
		_, err = h.registry.AddFile(ctx, versionInfoRepoFile, bytes.NewReader(versionInfoJSONBytes))
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to push VersionInfo JSON for %s@%s: %v", pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}
		publishedVersions[versionStr] = versionInfo
	}

	// 3. Update Dist-Tags
	// For each dist-tag, re-upload the tarball and versionInfo JSON using the dist-tag as OwningTag.
	// This will effectively update the manifest pointed to by that dist-tag.
	for distTag, versionStr := range pkgMeta.DistTags {
		versionInfo, viExists := pkgMeta.Versions[versionStr]
		attachmentFilename := ""
		var tarballBytes []byte

		if !viExists {
			// If version info is not in the current publish's versions map, it might be an existing version.
			// We cannot simply re-tag an OCI manifest with the current handler.Registry abstraction easily.
			// This part of dist-tag handling for existing versions is complex with AddFile.
			// For now, we only robustly support dist-tagging versions published in *this* request.
			// A real npm registry would allow pointing a dist-tag to any existing version.
			fmt.Printf("Dist-tag '%s' points to version '%s', which was not part of this publish's attachments. Skipping direct re-tagging of older versions for now.\n", distTag, versionStr)
			// To support this fully with AddFile, we'd need to:
			// 1. Fetch the tarball for 'versionStr' (if not in current attachments).
			// 2. Fetch the VersionInfo JSON for 'versionStr'.
			// 3. Then AddFile them with OwningTag = distTag.
			// This is too complex for this iteration.
			continue
		}

		// Find the attachment for this version to get its tarball data
		// This assumes versionStr from dist-tags matches a version just published.
		foundAttachment := false
		for attFilename, attStub := range pkgMeta.Attachments {
			// Try to match versionStr with version from this attachment's filename
			verFromAttFilename := ""
			matches := versionRegex.FindStringSubmatch(attFilename)
			if len(matches) == 3 { verFromAttFilename = matches[2] } else {
				simpleVersionRegex := regexp.MustCompile(`^[^/]+?-(\d+\.\d+\.\d+(?:-[^{}+]+(?:\.[^{}+]+)*)?(?:[+]{1}[^{}\s]+)?)\.tgz$`)
				simpleMatches := simpleVersionRegex.FindStringSubmatch(attFilename)
				if len(simpleMatches) == 2 { verFromAttFilename = simpleMatches[1] }
			}

			if verFromAttFilename == versionStr {
				var err error
				tarballBytes, err = base64.StdEncoding.DecodeString(attStub.Data)
				if err != nil {
					fmt.Printf("Error decoding tarball for dist-tag %s (version %s): %v. Skipping this dist-tag.\n", distTag, versionStr, err)
					tarballBytes = nil // Ensure it's nil
					break
				}
				attachmentFilename = attFilename
				foundAttachment = true
				break
			}
		}

		if !foundAttachment || tarballBytes == nil {
			fmt.Printf("Tarball for version %s (for dist-tag %s) not found in current attachments. Skipping this dist-tag update.\n", versionStr, distTag)
			continue
		}
		
		// Push Tarball for the dist-tag
		distTagTarballFile := &oci.RepoFile{
			OwningRepo: ociRepoName,
			OwningTag:  distTag, // Tag the manifest with the dist-tag (e.g., "latest")
			Name:       attachmentFilename, // Use the original filename
			MediaType:  TarballArtifactType, // Use the more specific TarballArtifactType
		}
		if _, err := h.registry.AddFile(ctx, distTagTarballFile, bytes.NewReader(tarballBytes)); err != nil {
			http.Error(w, fmt.Sprintf("failed to push tarball for dist-tag %s (%s@%s): %v", distTag, pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}

		// Push VersionInfo JSON for the dist-tag
		versionInfoJSONBytes, err := json.Marshal(versionInfo)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to marshal VersionInfo for dist-tag %s (%s@%s): %v", distTag, pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}
		distTagVersionInfoFile := &oci.RepoFile{
			OwningRepo: ociRepoName,
			OwningTag:  distTag, // Add to the same manifest tagged with distTag
			Name:       VersionInfoFilename,
			MediaType:  ArtifactType,
		}
		if _, err := h.registry.AddFile(ctx, distTagVersionInfoFile, bytes.NewReader(versionInfoJSONBytes)); err != nil {
			http.Error(w, fmt.Sprintf("failed to push VersionInfo for dist-tag %s (%s@%s): %v", distTag, pkgNameFromURL, versionStr, err), http.StatusInternalServerError)
			return
		}
		fmt.Printf("Successfully updated dist-tag '%s' to point to version '%s' for package '%s'\n", distTag, versionStr, pkgNameFromURL)
	}

	// 4. Response
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated) // 201 Created for successful publish
	// The _rev field is CouchDB specific. OCI doesn't have a direct equivalent for the whole package.
	// We could use a hash of the dist-tags map or similar if needed. For now, omitting.
	if err := json.NewEncoder(w).Encode(npmdata.ModifyResponse{Ok: true, ID: pkgNameFromURL}); err != nil {
		// Log error, headers already sent
		fmt.Printf("Error encoding success response for %s: %v\n", pkgNameFromURL, err)
	}
}

func unpublishPackageHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"]
	// revision := vars["revision"] // Revision is mostly for CouchDB compatibility, not directly used for OCI tag deletion.
	filename, hasFilename := vars["filename"]
	ctx := req.Context()

	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	if hasFilename {
		// Specific version unpublish
		var versionStr string
		matches := versionRegex.FindStringSubmatch(filename)
		if len(matches) == 3 {
			versionStr = matches[2]
		} else {
			// Fallback for simpler filenames if necessary, though versionRegex should handle most.
			simpleVersionRegex := regexp.MustCompile(`^[^/]+?-(\d+\.\d+\.\d+(?:-[^{}+]+(?:\.[^{}+]+)*)?(?:[+]{1}[^{}\s]+)?)\.tgz$`)
			simpleMatches := simpleVersionRegex.FindStringSubmatch(filename)
			if len(simpleMatches) == 2 {
				versionStr = simpleMatches[1]
			} else {
				http.Error(w, fmt.Sprintf("Could not parse version from filename: %s", filename), http.StatusBadRequest)
				return
			}
		}

		if versionStr == "" { // Should be caught by above checks, but as a safeguard.
			http.Error(w, fmt.Sprintf("Could not determine version from filename: %s", filename), http.StatusBadRequest)
			return
		}
		
		err := h.registry.DeleteTagFiles(ctx, ociRepoName, versionStr)
		if err != nil {
			if errors.IsOCINotFound(err) { // Assuming DeleteTagFiles or its underlying calls might return an OCI Not Found error
				http.Error(w, fmt.Sprintf("Version %s for package %s not found: %v", versionStr, pkgNameFromURL, err), http.StatusNotFound)
			} else {
				http.Error(w, fmt.Sprintf("Failed to unpublish version %s for package %s: %v", versionStr, pkgNameFromURL, err), http.StatusInternalServerError)
			}
			return
		}

		// TODO: More sophisticated dist-tag handling. If 'latest' or other dist-tags pointed to this version,
		// they are now stale or will resolve to nothing. getPackageMetadataHandler recalculates 'latest'
		// based on remaining semvers, which is a partial solution.

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(npmdata.ModifyResponse{Ok: true}); err != nil {
			fmt.Printf("Error encoding unpublish success response for %s@%s: %v\n", pkgNameFromURL, versionStr, err)
		}

	} else {
		// Entire package unpublish - Not implemented for this task
		// A real implementation would need to list all tags via h.registry.ListTags(ctx, ociRepoName)
		// and then call h.registry.DeleteTagFiles for each tag.
		// This is destructive and needs careful consideration.
		http.Error(w, "Unpublishing an entire package is not implemented", http.StatusNotImplemented)
	}
}

func distTagAddHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"]
	distTagName := vars["tag"]
	ctx := req.Context()

	// Read the version string from the request body. Expected to be a simple JSON string like "1.0.0".
	var versionStr string
	if err := json.NewDecoder(req.Body).Decode(&versionStr); err != nil {
		http.Error(w, fmt.Sprintf("Failed to parse version string from request body: %v", err), http.StatusBadRequest)
		return
	}
	defer req.Body.Close()

	if versionStr == "" {
		http.Error(w, "Version string in request body cannot be empty", http.StatusBadRequest)
		return
	}

	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	// Verify the target versionStr exists as a manifest/tag
	_, err := h.registry.Resolve(ctx, ociRepoName, versionStr)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("Target version %s not found for package %s", versionStr, pkgNameFromURL), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("Failed to resolve target version %s for package %s: %v", versionStr, pkgNameFromURL, err), http.StatusInternalServerError)
		}
		return
	}

	// Create/update the dist-tag to point to the versionStr's manifest
	err = h.registry.TagManifest(ctx, ociRepoName, versionStr, distTagName)
	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to set dist-tag %s to version %s for package %s: %v", distTagName, versionStr, pkgNameFromURL, err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]any{"ok": true, "message": fmt.Sprintf("Tag %s set to %s", distTagName, versionStr)}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		fmt.Printf("Error encoding dist-tag add success response for %s: %v\n", pkgNameFromURL, err)
	}
}

func distTagRmHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"]
	distTagName := vars["tag"]
	ctx := req.Context()

	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	err := h.registry.DeleteTag(ctx, ociRepoName, distTagName)
	if err != nil {
		if errors.IsOCINotFound(err) {
			http.Error(w, fmt.Sprintf("Dist-tag %s not found for package %s: %v", distTagName, pkgNameFromURL, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("Failed to remove dist-tag %s for package %s: %v", distTagName, pkgNameFromURL, err), http.StatusInternalServerError)
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	response := map[string]any{"ok": true, "message": fmt.Sprintf("Tag %s removed", distTagName)}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		fmt.Printf("Error encoding dist-tag remove success response for %s: %v\n", pkgNameFromURL, err)
	}
}

func distTagLsHandler(w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	pkgNameFromURL := vars["package"]
	ctx := req.Context()

	ociRepoName := RepoType + "/" + strings.Replace(pkgNameFromURL, "@", "", 1)

	allTags, err := h.registry.ListTags(ctx, ociRepoName)
	if err != nil {
		if errors.IsOCINotFound(err) { // Assuming ListTags can also indicate a repo not found
			http.Error(w, fmt.Sprintf("Package %s not found or has no tags: %v", pkgNameFromURL, err), http.StatusNotFound)
		} else {
			http.Error(w, fmt.Sprintf("Failed to list tags for package %s: %v", pkgNameFromURL, err), http.StatusInternalServerError)
		}
		return
	}

	distTagsMap := make(map[string]string)
	potentialDistTags := []string{}
	versionTagsAndDigests := make(map[string]ocispec.Descriptor)

	// Separate semver tags and potential dist-tags, and resolve semver tags
	for _, tag := range allTags {
		sv, err := semver.NewVersion(tag)
		if err != nil { // Not a valid semver, so it's a potential dist-tag
			potentialDistTags = append(potentialDistTags, tag)
		} else { // Valid semver
			desc, err := h.registry.Resolve(ctx, ociRepoName, sv.Original())
			if err == nil {
				versionTagsAndDigests[sv.Original()] = desc
			} else {
				fmt.Printf("Warning: could not resolve semver tag %s for package %s: %v\n", sv.Original(), pkgNameFromURL, err)
			}
		}
	}
	
	// For each potential dist-tag, find which version tag it points to by comparing manifest digests
	for _, distTag := range potentialDistTags {
		distTagDesc, err := h.registry.Resolve(ctx, ociRepoName, distTag)
		if err != nil {
			fmt.Printf("Warning: could not resolve potential dist-tag %s for package %s: %v\n", distTag, pkgNameFromURL, err)
			continue
		}

		for version, versionDesc := range versionTagsAndDigests {
			if versionDesc.Digest == distTagDesc.Digest {
				distTagsMap[distTag] = version
				break 
			}
		}
	}
	
	// Ensure _id is part of the response as per npm package metadata GET response for dist-tags
	// Although npm CLI for `npm dist-tag ls` just expects the map directly.
	// For CouchDB compatibility an _id and _rev might be there, but for OCI, the map is sufficient.
	// The npm CLI `dist-tag ls` command just prints the key-value pairs.
	// If the map is empty, an empty JSON object {} is fine.

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(distTagsMap); err != nil {
		fmt.Printf("Error encoding dist-tag list success response for %s: %v\n", pkgNameFromURL, err)
	}
}

func pingHandler(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
