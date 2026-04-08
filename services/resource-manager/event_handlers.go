package main

// event_handlers.go — HTTP handler for instance event history.
//
// M7 scope: GET /v1/instances/{id}/events.
//
// Ownership is enforced before returning events — the instance must belong
// to the calling principal (same 404-on-mismatch rule as other instance sub-resources).
//
// Source: EVENTS_SCHEMA_V1 §2, 09-01 §Event History Card,
//         AUTH_OWNERSHIP_MODEL_V1 §3.

import (
	"net/http"
)

// handleListEvents handles GET /v1/instances/{id}/events.
// Returns up to 100 events for the instance, newest first.
//
// This method is called from handleInstanceByID when subpath == "events".
// Auth (requirePrincipal) is already applied by the parent handler.
//
// Source: EVENTS_SCHEMA_V1 §4, 09-01 §Event History Card.
func (s *server) handleListEvents(w http.ResponseWriter, r *http.Request, instanceID string) {
	principal, _ := principalFromCtx(r.Context())

	// Enforce ownership. 404 on any miss or cross-account access.
	// Source: AUTH_OWNERSHIP_MODEL_V1 §3.
	_, ok := s.loadOwnedInstance(w, r, principal, instanceID)
	if !ok {
		return
	}

	rows, err := s.repo.ListEvents(r.Context(), instanceID, 100)
	if err != nil {
		s.log.Error("ListEvents failed", "instance_id", instanceID, "error", err)
		writeInternalError(w)
		return
	}

	out := make([]EventResponse, 0, len(rows))
	for _, row := range rows {
		ev := EventResponse{
			ID:        row.ID,
			EventType: row.EventType,
			CreatedAt: row.CreatedAt,
		}
		if row.Message != "" {
			ev.Message = &row.Message
		}
		if row.Actor != "" {
			ev.Actor = &row.Actor
		}
		out = append(out, ev)
	}

	writeJSON(w, http.StatusOK, ListEventsResponse{
		Events: out,
		Total:  len(out),
	})
}
