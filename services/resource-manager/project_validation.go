package main

// project_validation.go — Request field validation for project create/update.
//
// Uses the existing fieldErr type and error constants from instance_errors.go.
// No new error codes introduced.
//
// Source: P2_PROJECT_RBAC_MODEL.md §2.3 (field constraints),
//         API_ERROR_CONTRACT_V1 §4, §6 (validation execution order).

import (
	"regexp"
	"strings"
)

// projectNameRE enforces the project name format.
// Matches the instance name pattern per P2_PROJECT_RBAC_MODEL.md §2.3:
// ^[a-z][a-z0-9-]{0,61}[a-z0-9]$
var projectNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,61}[a-z0-9]$`)

// validateProjectCreate validates a createProjectRequest.
// Returns one fieldErr per invalid field; nil if valid.
func validateProjectCreate(req *createProjectRequest) []fieldErr {
	var errs []fieldErr
	errs = append(errs, validateProjectNameField(req.Name)...)
	errs = append(errs, validateProjectDisplayNameField(req.DisplayName)...)
	return errs
}

// validateProjectUpdate validates an updateProjectRequest.
// Returns one fieldErr per invalid field; nil if valid.
func validateProjectUpdate(req *updateProjectRequest) []fieldErr {
	var errs []fieldErr
	errs = append(errs, validateProjectNameField(req.Name)...)
	errs = append(errs, validateProjectDisplayNameField(req.DisplayName)...)
	return errs
}

func validateProjectNameField(name string) []fieldErr {
	if strings.TrimSpace(name) == "" {
		return []fieldErr{{errMissingField, "The field 'name' is required.", "name"}}
	}
	if !projectNameRE.MatchString(name) {
		return []fieldErr{{errInvalidName,
			"name must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.", "name"}}
	}
	return nil
}

func validateProjectDisplayNameField(displayName string) []fieldErr {
	if strings.TrimSpace(displayName) == "" {
		return []fieldErr{{errMissingField, "The field 'display_name' is required.", "display_name"}}
	}
	if len(displayName) > 128 {
		return []fieldErr{{errInvalidValue,
			"display_name must be 128 characters or fewer.", "display_name"}}
	}
	return nil
}
