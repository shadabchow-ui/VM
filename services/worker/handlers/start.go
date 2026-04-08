package handlers

// start.go — INSTANCE_START handler stub. M3 deliverable.
// Source: 04-02-lifecycle-action-flows.md §INSTANCE_START.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
)

// StartHandler handles INSTANCE_START jobs. M3 deliverable.
type StartHandler struct{ deps *Deps; log *slog.Logger }

func NewStartHandler(deps *Deps, log *slog.Logger) *StartHandler {
	return &StartHandler{deps: deps, log: log}
}

func (h *StartHandler) Execute(_ context.Context, job *db.JobRow) error {
	h.log.Error("INSTANCE_START not yet implemented — M3 deliverable", "job_id", job.ID)
	return fmt.Errorf("INSTANCE_START: not implemented until M3 gate")
}
