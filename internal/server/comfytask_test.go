package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// fakeComfyUpstream is a minimal ComfyUI stand-in whose job state can be set
// per test to drive terminal-status backfill.
type fakeComfyUpstream struct {
	srv    *httptest.Server
	status comfy.JobStatus
}

func newFakeComfyUpstream(status comfy.JobStatus) *fakeComfyUpstream {
	f := &fakeComfyUpstream{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		// Never in the queue: terminal state is read from /history.
		_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		msg := `[]`
		switch f.status {
		case comfy.StatusFailed:
			msg = `[["execution_error",{}]]`
		case comfy.StatusCancelled:
			msg = `[["execution_interrupted",{}]]`
		}
		_, _ = w.Write([]byte(`{"abc":{"status":{"completed":true,"status_str":"success","messages":` + msg + `}}}`))
	})
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeComfyUpstream) close() { f.srv.Close() }

func TestComfy_ExtractPromptID(t *testing.T) {
	cases := map[string]string{
		`{"prompt_id":"abc","number":1,"node_errors":{}}`: "abc",
		`{"error":"no_prompt","node_errors":{}}`:          "",
		`not json`:                                        "",
		``:                                                "",
	}
	for body, want := range cases {
		if got := extractComfyPromptID([]byte(body)); got != want {
			t.Errorf("extractComfyPromptID(%q) = %q, want %q", body, got, want)
		}
	}
}

func TestComfy_IsPromptSubmit(t *testing.T) {
	cases := []struct {
		method, path string
		want         bool
	}{
		// serveComfyUI sees the root-relative path (the /comfyui prefix is
		// stripped, and the dedicated port is already root-relative).
		{http.MethodPost, "/prompt", true},
		{http.MethodPost, "/prompt/", true},
		{http.MethodGet, "/prompt", false},
		{http.MethodPost, "/history", false},
		{http.MethodPost, "/interrupt", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, c.path, nil)
		if got := isComfyPromptSubmit(r); got != c.want {
			t.Errorf("isComfyPromptSubmit(%s %s) = %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

// TestComfyTask_TrackRecordsRowAndBackfills drives a full track: the submit
// inserts a running row, then the tracker polls and backfills the terminal
// status and duration.
func TestComfyTask_TrackRecordsRowAndBackfills(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusCompleted)
	defer fc.close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	row := store.ActivityLogEntry{
		Timestamp:      time.Now().Add(-5 * time.Second),
		Model:          config.ComfyUIModelID,
		ReqPath:        comfyPromptPath,
		RespStatusCode: http.StatusOK,
	}
	tracker := newComfyTaskTracker(s)
	tracker.track(row, "abc")

	// The initial row is inserted synchronously with a running status.
	entries := listActivity(t, s)
	if len(entries) != 1 {
		t.Fatalf("activity rows = %d, want 1", len(entries))
	}
	if entries[0].Metadata[metaComfyStatus] != string(comfy.StatusRunning) {
		t.Errorf("initial status = %q, want running", entries[0].Metadata[metaComfyStatus])
	}

	// Wait for the tracker goroutine to backfill the terminal status.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries = listActivity(t, s)
		if entries[0].Metadata[metaComfyStatus] == string(comfy.StatusCompleted) {
			if entries[0].DurationMs <= 0 {
				t.Errorf("DurationMs = %d, want > 0 after backfill", entries[0].DurationMs)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status never backfilled to completed; got %q", entries[0].Metadata[metaComfyStatus])
}

// TestComfyTask_BackfillFailed proves a failed workflow records its status and
// an error message.
func TestComfyTask_BackfillFailed(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusFailed)
	defer fc.close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	newComfyTaskTracker(s).track(store.ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     config.ComfyUIModelID,
		ReqPath:   comfyPromptPath,
	}, "abc")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries := listActivity(t, s)
		if entries[0].Metadata[metaComfyStatus] == string(comfy.StatusFailed) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status never backfilled to failed")
}

// TestComfyTask_NoPromptIDIsNoop proves tracking does nothing without a prompt
// id (e.g. a validation-error submit).
func TestComfyTask_NoPromptIDIsNoop(t *testing.T) {
	s := newTestServerWithConfig(config.Config{}, nil, nil)
	defer s.store.Close()

	newComfyTaskTracker(s).track(store.ActivityLogEntry{Model: config.ComfyUIModelID}, "")
	if entries := listActivity(t, s); len(entries) != 0 {
		t.Errorf("activity rows = %d, want 0 for empty prompt id", len(entries))
	}
}

func listActivity(t *testing.T, s *Server) []store.ActivityLogEntry {
	t.Helper()
	page, err := s.store.Activity().List(context.Background(), store.ActivityQuery{})
	if err != nil {
		t.Fatalf("activity list: %v", err)
	}
	return page.Data
}
