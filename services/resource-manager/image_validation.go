package main

// image_validation.go — Request validation for custom image endpoints.
//
// VM-P2C-P2: validateCreateImageFromSnapshotRequest, validateImportImageRequest.
// VM-P2C-P3: added family_name / family_version field validation in both request
//            validators; added validateFamilyName helper.
//
// Follows the same validation style as snapshot_validation.go and volume_validation.go:
//   - Returns []fieldErr (reuses the existing type from instance_errors.go).
//   - Uses existing error codes where they apply (errMissingField, errInvalidName).
//   - Image-specific error codes are in image_errors.go.
//   - No new validation framework introduced.
//
// Cross-resource checks (snapshot existence, snapshot ownership, snapshot state)
// are done in the handlers after field validation returns no errors.
//
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6 (create rules), §3 (import rules),
//         API_ERROR_CONTRACT_V1.md §4, 08-02-validation-rules-and-error-contracts.md,
//         vm-13-01__blueprint__ §family_seam (family_name format).

import (
	"net/url"
	"regexp"
	"strings"
)

// familyNameRE is the allowed format for image family names.
// Family names follow the same slug convention as instance names: lowercase
// alphanumeric with hyphens, starting with a letter, 2–63 chars.
// Chosen to be safe as a URL path segment and DNS label component.
// Source: vm-13-01__blueprint__ §family_seam (family names are user-facing selection
// abstractions; they must be stable, human-readable identifiers).
var familyNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// validateFamilyName validates an optional family_name value.
// Returns a fieldErr if invalid; returns nil if valid or empty (not set).
// family_version without family_name is also an error.
func validateFamilyFields(familyName *string, familyVersion *int, fieldPrefix string) []fieldErr {
	var errs []fieldErr

	if familyName == nil {
		// No family specified — family_version alone is invalid.
		if familyVersion != nil {
			errs = append(errs, fieldErr{errImageFamilyInvalidRequest,
				"family_version requires family_name to be set.", fieldPrefix + "family_version"})
		}
		return errs
	}

	// family_name must match slug format.
	name := strings.TrimSpace(*familyName)
	if name == "" {
		errs = append(errs, fieldErr{errMissingField,
			"family_name must not be empty when specified.", fieldPrefix + "family_name"})
		return errs
	}
	if !familyNameRE.MatchString(name) {
		errs = append(errs, fieldErr{errInvalidName,
			"family_name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", fieldPrefix + "family_name"})
	}

	// family_version, when present, must be a positive integer.
	if familyVersion != nil && *familyVersion <= 0 {
		errs = append(errs, fieldErr{errInvalidValue,
			"family_version must be a positive integer when specified.", fieldPrefix + "family_version"})
	}

	return errs
}

// validateCreateImageFromSnapshotRequest validates POST /v1/images (snapshot source).
// Returns one fieldErr per failing field; nil if valid.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3.6.
func validateCreateImageFromSnapshotRequest(req *CreateImageFromSnapshotRequest) []fieldErr {
	var errs []fieldErr

	// name — required; reuse name regexp from instance_validation.go.
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if !nameRE.MatchString(req.Name) {
		errs = append(errs, fieldErr{errInvalidName,
			"Name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"})
	}

	// source_snapshot_id — required non-empty string.
	if strings.TrimSpace(req.SourceSnapshotID) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'source_snapshot_id' is required.", "source_snapshot_id"})
	}

	// os_family — required.
	if strings.TrimSpace(req.OSFamily) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'os_family' is required.", "os_family"})
	}

	// os_version — required.
	if strings.TrimSpace(req.OSVersion) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'os_version' is required.", "os_version"})
	}

	// architecture — required; must be a known value.
	if strings.TrimSpace(req.Architecture) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'architecture' is required.", "architecture"})
	} else if !validImageArchitecture(req.Architecture) {
		errs = append(errs, fieldErr{errInvalidValue,
			"architecture must be one of: x86_64, arm64.", "architecture"})
	}

	// min_disk_gb — optional; if set, must be positive.
	if req.MinDiskGB != nil && *req.MinDiskGB <= 0 {
		errs = append(errs, fieldErr{errInvalidValue,
			"min_disk_gb must be greater than 0 when specified.", "min_disk_gb"})
	}

	// family_name / family_version — optional; validated together.
	// VM-P2C-P3.
	errs = append(errs, validateFamilyFields(req.FamilyName, req.FamilyVersion, "")...)

	return errs
}

// validateImportImageRequest validates POST /v1/images (import source).
// Returns one fieldErr per failing field; nil if valid.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import flow).
func validateImportImageRequest(req *ImportImageRequest) []fieldErr {
	var errs []fieldErr

	// name — required; reuse name regexp from instance_validation.go.
	if strings.TrimSpace(req.Name) == "" {
		errs = append(errs, fieldErr{errMissingField, "The field 'name' is required.", "name"})
	} else if !nameRE.MatchString(req.Name) {
		errs = append(errs, fieldErr{errInvalidName,
			"Name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"})
	}

	// import_url — required; must be a parseable URL with http/https scheme.
	if strings.TrimSpace(req.ImportURL) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'import_url' is required.", "import_url"})
	} else if !validImportURL(req.ImportURL) {
		errs = append(errs, fieldErr{errImageImportURLInvalid,
			"import_url must be a valid http or https URL.", "import_url"})
	}

	// os_family — required.
	if strings.TrimSpace(req.OSFamily) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'os_family' is required.", "os_family"})
	}

	// os_version — required.
	if strings.TrimSpace(req.OSVersion) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'os_version' is required.", "os_version"})
	}

	// architecture — required; must be a known value.
	if strings.TrimSpace(req.Architecture) == "" {
		errs = append(errs, fieldErr{errMissingField,
			"The field 'architecture' is required.", "architecture"})
	} else if !validImageArchitecture(req.Architecture) {
		errs = append(errs, fieldErr{errInvalidValue,
			"architecture must be one of: x86_64, arm64.", "architecture"})
	}

	// min_disk_gb — required for imports; must be positive.
	if req.MinDiskGB <= 0 {
		errs = append(errs, fieldErr{errInvalidValue,
			"min_disk_gb must be greater than 0.", "min_disk_gb"})
	}

	// family_name / family_version — optional; validated together.
	// VM-P2C-P3.
	errs = append(errs, validateFamilyFields(req.FamilyName, req.FamilyVersion, "")...)

	return errs
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// validImageArchitecture reports whether the given architecture string is
// a known supported value for VM images.
// Source: INSTANCE_MODEL_V1.md §7 (Phase 1: x86_64 only; arm64 forward-compat).
var validArchitectures = map[string]bool{
	"x86_64": true,
	"arm64":  true,
}

func validImageArchitecture(arch string) bool {
	return validArchitectures[arch]
}

// validImportURL reports whether the given string is a parseable http/https URL.
// Source: P2_IMAGE_SNAPSHOT_MODEL.md §3 (import_url format).
func validImportURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
