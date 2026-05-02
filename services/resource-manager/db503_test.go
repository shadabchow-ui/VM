package main

// db503_test.go — Unit tests for DB-6 gate item (P2-M1/WS-H1).
//
// Tests:
//   TestWriteServiceUnavailable — verifies the 503 response shape.
//   TestIsDBUnavailableError    — verifies connectivity-class detection.
//   TestWriteDBError            — verifies writeDBError routing.
//
// Source: P2_M1_WS_H1_DB_HA_RUNBOOK §6 Step 11, API_ERROR_CONTRACT_V1 §1, §2, §7.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWriteServiceUnavailable verifies DB-6 pass conditions for the response shape:
//   - HTTP status is 503.
//   - Body field error.code == "service_unavailable".
//   - Body field error.request_id is non-empty.
//   - Body field error.details is present (empty array, not null).
func TestWriteServiceUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	writeServiceUnavailable(w)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503, got %d", w.Code)
	}

	var body struct {
		Error struct {
			Code      string        `json:"code"`
			Message   string        `json:"message"`
			RequestID string        `json:"request_id"`
			Details   []interface{} `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("response body is not valid JSON: %v", err)
	}
	if body.Error.Code != errServiceUnavailable {
		t.Errorf("error.code: want %q, got %q", errServiceUnavailable, body.Error.Code)
	}
	if body.Error.RequestID == "" {
		t.Error("error.request_id must not be empty (API_ERROR_CONTRACT_V1 §7)")
	}
	if body.Error.Details == nil {
		t.Error("error.details must be present (not null); want empty array")
	}
	if body.Error.Message == "" {
		t.Error("error.message must not be empty")
	}
}

// TestIsDBUnavailableError verifies that isDBUnavailableError correctly classifies
// transient connectivity errors as true and application-level errors as false.
func TestIsDBUnavailableError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Connectivity errors — must return true (→ 503).
		{"connection refused", errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"), true},
		{"connection reset", errors.New("read tcp: connection reset by peer"), true},
		{"broken pipe", errors.New("write: broken pipe"), true},
		{"unexpected eof", errors.New("unexpected EOF"), true},
		{"dial tcp timeout", errors.New("dial tcp: connection timed out"), true},
		{"io timeout", errors.New("i/o timeout"), true},
		{"no such host", errors.New("dial tcp: lookup db.internal: no such host"), true},
		{"server closed unexpectedly", errors.New("server closed the connection unexpectedly"), true},
		{"db starting up", errors.New("the database system is starting up"), true},

		// Application-level errors — must return false (→ 500 or handled elsewhere).
		{"duplicate key", errors.New("duplicate key value violates unique constraint \"instances_pkey\""), false},
		{"foreign key", errors.New("insert or update on table \"jobs\" violates foreign key constraint"), false},
		{"syntax error", errors.New("invalid input syntax for type uuid: \"bad\""), false},
		{"no rows", errors.New("no rows in result set"), false},
		{"nil error", nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isDBUnavailableError(tc.err)
			if got != tc.want {
				t.Errorf("isDBUnavailableError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWriteDBError verifies that writeDBError routes correctly:
//   - connectivity errors → 503
//   - application errors → 500
func TestWriteDBError(t *testing.T) {
	t.Run("connectivity error yields 503", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeDBError(w, errors.New("connection refused"))
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("want 503, got %d", w.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		json.NewDecoder(w.Body).Decode(&body) //nolint:errcheck
		if body.Error.Code != errServiceUnavailable {
			t.Errorf("want code=%q, got %q", errServiceUnavailable, body.Error.Code)
		}
	})

	t.Run("application error yields 500", func(t *testing.T) {
		w := httptest.NewRecorder()
		writeDBError(w, errors.New("duplicate key value violates unique constraint"))
		if w.Code != http.StatusInternalServerError {
			t.Errorf("want 500, got %d", w.Code)
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		json.NewDecoder(w.Body).Decode(&body) //nolint:errcheck
		if body.Error.Code != errInternalError {
			t.Errorf("want code=%q, got %q", errInternalError, body.Error.Code)
		}
	})
}
