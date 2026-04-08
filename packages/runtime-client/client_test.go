package runtimeclient

// client_test.go — Unit tests for the RuntimeService HTTP client.
//
// Uses net/http/httptest to simulate the Host Agent HTTP server.
// Tests cover: successful round-trips, HTTP error propagation,
// CreateInstance 300s timeout configuration, and ListInstances.
//
// Source: RUNTIMESERVICE_GRPC_V1 §2 (contract), IMPLEMENTATION_PLAN_V1 §C1.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── Test server helpers ───────────────────────────────────────────────────────

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	// Strip "http://" from URL — NewClient prepends it.
	addr := strings.TrimPrefix(srv.URL, "http://")
	client := NewClient("host-test", addr, nil)
	return srv, client
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// ── CreateInstance ────────────────────────────────────────────────────────────

func TestCreateInstance_HappyPath(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/runtime/v1/instances" {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		var req CreateInstanceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusOK, CreateInstanceResponse{
			InstanceID: req.InstanceID,
			State:      "RUNNING",
		})
	})

	resp, err := client.CreateInstance(context.Background(), &CreateInstanceRequest{
		InstanceID: "inst_test001",
		CPUCores:   2,
		MemoryMB:   4096,
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if resp.InstanceID != "inst_test001" {
		t.Errorf("InstanceID = %q, want inst_test001", resp.InstanceID)
	}
	if resp.State != "RUNNING" {
		t.Errorf("State = %q, want RUNNING", resp.State)
	}
}

func TestCreateInstance_PropagatesHTTPError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "host agent error", http.StatusInternalServerError)
	})

	_, err := client.CreateInstance(context.Background(), &CreateInstanceRequest{InstanceID: "inst_x"})
	if err == nil {
		t.Error("expected error on 500 response, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %v, expected to contain '500'", err)
	}
}

func TestCreateInstance_RequestBodyContainsInstanceID(t *testing.T) {
	var captured CreateInstanceRequest
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		writeJSON(w, http.StatusOK, CreateInstanceResponse{InstanceID: captured.InstanceID, State: "RUNNING"})
	})

	_, _ = client.CreateInstance(context.Background(), &CreateInstanceRequest{
		InstanceID: "inst_body_check",
		CPUCores:   4,
		MemoryMB:   8192,
		Network:    NetworkConfig{PrivateIP: "10.0.0.5", TapDevice: "tap-abc"},
	})

	if captured.InstanceID != "inst_body_check" {
		t.Errorf("captured InstanceID = %q, want inst_body_check", captured.InstanceID)
	}
	if captured.CPUCores != 4 {
		t.Errorf("captured CPUCores = %d, want 4", captured.CPUCores)
	}
	if captured.Network.PrivateIP != "10.0.0.5" {
		t.Errorf("captured Network.PrivateIP = %q, want 10.0.0.5", captured.Network.PrivateIP)
	}
}

func TestCreateInstance_UsesLongTimeout(t *testing.T) {
	// Verify the createInstanceTimeout constant is >= 300s.
	if createInstanceTimeout < 300*time.Second {
		t.Errorf("createInstanceTimeout = %v, want >= 300s", createInstanceTimeout)
	}
}

// ── StopInstance ──────────────────────────────────────────────────────────────

func TestStopInstance_HappyPath(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runtime/v1/instances/stop" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var req StopInstanceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusOK, StopInstanceResponse{InstanceID: req.InstanceID, State: "STOPPED"})
	})

	resp, err := client.StopInstance(context.Background(), &StopInstanceRequest{
		InstanceID:     "inst_stop001",
		TimeoutSeconds: 30,
	})
	if err != nil {
		t.Fatalf("StopInstance: %v", err)
	}
	if resp.State != "STOPPED" {
		t.Errorf("State = %q, want STOPPED", resp.State)
	}
}

func TestStopInstance_PropagatesHTTPError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "host agent unavailable", http.StatusServiceUnavailable)
	})

	_, err := client.StopInstance(context.Background(), &StopInstanceRequest{InstanceID: "inst_x"})
	if err == nil {
		t.Error("expected error on 503, got nil")
	}
}

// ── DeleteInstance ────────────────────────────────────────────────────────────

func TestDeleteInstance_HappyPath(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/runtime/v1/instances/delete" {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		var req DeleteInstanceRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		writeJSON(w, http.StatusOK, DeleteInstanceResponse{InstanceID: req.InstanceID, State: "DELETED"})
	})

	resp, err := client.DeleteInstance(context.Background(), &DeleteInstanceRequest{
		InstanceID:     "inst_del001",
		DeleteRootDisk: true,
	})
	if err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	if resp.State != "DELETED" {
		t.Errorf("State = %q, want DELETED", resp.State)
	}
}

func TestDeleteInstance_DeleteRootDiskSentInBody(t *testing.T) {
	var captured DeleteInstanceRequest
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&captured)
		writeJSON(w, http.StatusOK, DeleteInstanceResponse{InstanceID: "inst_x", State: "DELETED"})
	})

	_, _ = client.DeleteInstance(context.Background(), &DeleteInstanceRequest{
		InstanceID:     "inst_x",
		DeleteRootDisk: true,
	})

	if !captured.DeleteRootDisk {
		t.Error("delete_root_disk not sent in request body (Phase 1: must always be true)")
	}
}

// ── ListInstances ─────────────────────────────────────────────────────────────

func TestListInstances_HappyPath(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/runtime/v1/instances" {
			http.Error(w, "wrong method/path", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, ListInstancesResponse{
			Instances: []InstanceStatus{
				{InstanceID: "inst_a", State: "RUNNING", HostPID: 12345},
				{InstanceID: "inst_b", State: "STOPPED", HostPID: 0},
			},
		})
	})

	resp, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.Instances) != 2 {
		t.Fatalf("Instances count = %d, want 2", len(resp.Instances))
	}
	if resp.Instances[0].State != "RUNNING" {
		t.Errorf("Instances[0].State = %q, want RUNNING", resp.Instances[0].State)
	}
	if resp.Instances[0].HostPID != 12345 {
		t.Errorf("Instances[0].HostPID = %d, want 12345", resp.Instances[0].HostPID)
	}
}

func TestListInstances_EmptyList(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ListInstancesResponse{Instances: nil})
	})

	resp, err := client.ListInstances(context.Background())
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.Instances) != 0 {
		t.Errorf("Instances count = %d, want 0", len(resp.Instances))
	}
}

// ── Error propagation ─────────────────────────────────────────────────────────

func TestClient_ContextCancellation_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Never respond — let context cancel first.
		<-r.Context().Done()
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	client := NewClient("host-test", addr, &http.Client{Timeout: 50 * time.Millisecond})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	_, err := client.ListInstances(ctx)
	if err == nil {
		t.Error("expected error on cancelled context, got nil")
	}
}

func TestClient_MalformedJSONResponse_ReturnsError(t *testing.T) {
	_, client := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json {{{{"))
	})

	_, err := client.ListInstances(context.Background())
	if err == nil {
		t.Error("expected error on malformed JSON, got nil")
	}
}
