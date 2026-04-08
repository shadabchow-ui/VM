package handlers

// stop.go — INSTANCE_STOP handler stub. M3 deliverable.
// Source: 04-02-lifecycle-action-flows.md §INSTANCE_STOP.

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
)

// StopHandler handles INSTANCE_STOP jobs. M3 deliverable.
type StopHandler struct{ deps *Deps; log *slog.Logger }

func NewStopHandler(deps *Deps, log *slog.Logger) *StopHandler {
	return &StopHandler{deps: deps, log: log}
}

func (h *StopHandler) Execute(_ context.Context, job *db.JobRow) error {
	h.log.Error("INSTANCE_STOP not yet implemented — M3 deliverable", "job_id", job.ID)
	return fmt.Errorf("INSTANCE_STOP: not implemented until M3 gate")
}
