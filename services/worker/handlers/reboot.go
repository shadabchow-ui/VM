package handlers

// reboot.go — INSTANCE_REBOOT handler stub. M3 deliverable.
// Source: 04-02-lifecycle-action-flows.md §INSTANCE_REBOOT.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
)

// RebootHandler handles INSTANCE_REBOOT jobs. M3 deliverable.
type RebootHandler struct{ deps *Deps; log *slog.Logger }

func NewRebootHandler(deps *Deps, log *slog.Logger) *RebootHandler {
	return &RebootHandler{deps: deps, log: log}
}

func (h *RebootHandler) Execute(_ context.Context, job *db.JobRow) error {
	h.log.Error("INSTANCE_REBOOT not yet implemented — M3 deliverable", "job_id", job.ID)
	return fmt.Errorf("INSTANCE_REBOOT: not implemented until M3 gate")
}
