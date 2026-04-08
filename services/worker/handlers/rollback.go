package handlers

// rollback.go — Standalone provisioning rollback.
//
// Source: IMPLEMENTATION_PLAN_V1 §38, 04-04-provisioning-failure-handling-and-rollback.md.
// R-06: compensating actions in exact reverse-allocation order.
// All steps are idempotent — continue on partial failure.

import (
	"context"
	"log/slog"

	"github.com/compute-platform/compute-platform/internal/db"
	"github.com/compute-platform/compute-platform/packages/idgen"
	runtimeclient "github.com/compute-platform/compute-platform/packages/runtime-client"
)

// RollbackProvisioning releases all resources from a failed or stuck INSTANCE_CREATE.
// Called inline by CreateHandler and by the reconciler for stuck provisioning instances.
func RollbackProvisioning(
	ctx context.Context,
	store InstanceStore,
	network NetworkController,
	runtimeFactory func(hostID, addr string) *runtimeclient.Client,
	instanceID, hostID, hostAddr, allocatedIP, vpcID string,
	log *slog.Logger,
) {
	log = log.With("instance_id", instanceID, "rollback", true)
	log.Warn("rollback: begin")

	// R1: Delete VM process + TAP + rootfs on host agent (idempotent).
	if hostID != "" && hostAddr != "" {
		rtClient := RuntimeClient(runtimeFactory(hostID, hostAddr))
		if _, err := rtClient.DeleteInstance(ctx, &runtimeclient.DeleteInstanceRequest{
			InstanceID: instanceID, DeleteRootDisk: true,
		}); err != nil {
			log.Error("rollback R1: DeleteInstance failed — continuing", "error", err)
		} else {
			log.Info("rollback R1: VM resources deleted")
		}
	}

	// R2: Release IP (idempotent).
	if allocatedIP != "" && vpcID != "" {
		if err := network.ReleaseIP(ctx, allocatedIP, vpcID, instanceID); err != nil {
			log.Error("rollback R2: ReleaseIP failed — IP may be leaked", "ip", allocatedIP, "error", err)
		} else {
			log.Info("rollback R2: IP released", "ip", allocatedIP)
		}
		_ = store.InsertEvent(ctx, &db.EventRow{
			ID:         idgen.New(idgen.PrefixEvent),
			InstanceID: instanceID,
			EventType:  db.EventIPReleased,
			Message:    "IP released during rollback: " + allocatedIP,
			Actor:      "system",
		})
	}

	// R3: Transition instance to failed.
	inst, err := store.GetInstanceByID(ctx, instanceID)
	if err != nil {
		log.Error("rollback R3: could not load instance", "error", err)
		return
	}
	if inst.VMState != "failed" && inst.VMState != "deleted" {
		if err := store.UpdateInstanceState(ctx, instanceID, inst.VMState, "failed", inst.Version); err != nil {
			log.Error("rollback R3: could not set failed", "error", err)
		} else {
			log.Info("rollback R3: instance marked failed")
		}
		_ = store.InsertEvent(ctx, &db.EventRow{
			ID:         idgen.New(idgen.PrefixEvent),
			InstanceID: instanceID,
			EventType:  db.EventInstanceFailure,
			Message:    "Provisioning rolled back",
			Actor:      "system",
		})
	}
	log.Warn("rollback: complete")
}
