package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// fakeComfyUpstream is a minimal ComfyUI stand-in whose job state can be set
// per test to drive terminal-status backfill.
type fakeComfyUpstream struct {
	srv       *httptest.Server
	status    comfy.JobStatus
	liveQueue string
}

func newFakeComfyUpstream(status comfy.JobStatus) *fakeComfyUpstream {
	f := &fakeComfyUpstream{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		if f.liveQueue != "" {
			_, _ = w.Write([]byte(f.liveQueue))
			return
		}
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
		// Key the entry by the requested prompt id so JobState finds it.
		id := strings.TrimPrefix(r.URL.Path, "/history/")
		_, _ = w.Write([]byte(`{"` + id + `":{"status":{"completed":true,"status_str":"success","messages":` + msg + `}}}`))
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

// fakeDirectProcess is a minimal process.Process that records ServeHTTP calls,
// standing in for the real process behind DirectProcess.
type fakeDirectProcess struct {
	serveCalls atomic.Int32
	response   string
}

func (f *fakeDirectProcess) Run(time.Duration) error         { return nil }
func (f *fakeDirectProcess) WaitReady(context.Context) error { return nil }
func (f *fakeDirectProcess) EnsureReady(context.Context, time.Duration) error {
	return nil
}
func (f *fakeDirectProcess) Stop(time.Duration) error    { return nil }
func (f *fakeDirectProcess) State() process.ProcessState { return process.StateReady }
func (f *fakeDirectProcess) Status() process.Status      { return process.Status{State: process.StateReady} }
func (f *fakeDirectProcess) Logger() *logmon.Monitor     { return nil }
func (f *fakeDirectProcess) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.serveCalls.Add(1)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(f.response))
}

// TestComfy_DirectClassification proves only a workflow submit (POST /prompt)
// is treated as swappable work; every other ComfyUI request is direct and must
// never trigger a swap. This includes settings writes like the frontend's
// POST /api/settings/... which previously fell through to the scheduler and
// unloaded a running LLM.
func TestComfy_DirectClassification(t *testing.T) {
	direct := []struct{ method, path string }{
		{http.MethodGet, "/queue"},
		{http.MethodGet, "/history/abc"},
		{http.MethodGet, "/api/jobs"},
		{http.MethodGet, "/system_stats"},
		{http.MethodGet, "/view"},
		{http.MethodGet, "/object_info"},
		{http.MethodPost, "/interrupt"},
		{http.MethodPost, "/free"},
		{http.MethodPost, "/api/jobs/abc/cancel"},
		{http.MethodPost, "/api/jobs/cancel"},
		{http.MethodPost, "/api/settings/Comfy.Queue.History.Expanded"},
		{http.MethodPost, "/upload/image"},
		{http.MethodPost, "/history"},
		{http.MethodDelete, "/history"},
	}
	for _, c := range direct {
		r := httptest.NewRequest(c.method, c.path, nil)
		if !isComfyDirect(r) {
			t.Errorf("isComfyDirect(%s %s) = false, want true (must not swap)", c.method, c.path)
		}
		if isComfyPromptSubmit(r) {
			t.Errorf("isComfyPromptSubmit(%s %s) = true, want false", c.method, c.path)
		}
	}
	// Only the workflow submit is swappable work.
	submit := httptest.NewRequest(http.MethodPost, "/prompt", nil)
	if isComfyDirect(submit) {
		t.Errorf("isComfyDirect(POST /prompt) = true, want false (it is the only swapper)")
	}
}

// TestServer_ComfyDirect_NeverSwaps is the regression guard for the spurious
// swap: a non-submit request (e.g. a settings write from the ComfyUI UI) must
// be served straight to the ready process and must NOT reach the scheduler,
// so it can never evict a running LLM.
func TestServer_ComfyDirect_NeverSwaps(t *testing.T) {
	proc := &fakeDirectProcess{response: `{}`}
	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	local.directProcess = proc
	// If the scheduler were reached, serveHTTP would fire. It must not.
	schedulerReached := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		schedulerReached = true
		w.WriteHeader(http.StatusOK)
	}

	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{config.ComfyUIModelID: {}}}
	s.routes()

	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/comfyui/api/settings/Comfy.Queue.History.Expanded", strings.NewReader("true")),
		httptest.NewRequest(http.MethodGet, "/comfyui/history", nil),
		httptest.NewRequest(http.MethodGet, "/comfyui/view", nil),
		httptest.NewRequest(http.MethodPost, "/comfyui/interrupt", nil),
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
	}

	if schedulerReached {
		t.Errorf("a direct ComfyUI request reached the scheduler; it must never swap")
	}
	if got := proc.serveCalls.Load(); got != 4 {
		t.Errorf("direct ServeHTTP calls = %d, want 4", got)
	}
}

// TestServer_ComfyDirect_StartsWithoutSwapping proves a direct request to a
// ComfyUI that is not yet running starts it and holds (or reports 503 if the
// start fails) WITHOUT ever reaching the scheduler — so it can never evict a
// running LLM.
func TestServer_ComfyDirect_StartsWithoutSwapping(t *testing.T) {
	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	local.notReady = true // start will fail
	schedulerReached := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		schedulerReached = true
		w.WriteHeader(http.StatusOK)
	}

	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{config.ComfyUIModelID: {}}}
	s.routes()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/history", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (start failed)", w.Code)
	}
	if schedulerReached {
		t.Errorf("a direct request reached the scheduler; it must never swap")
	}
}

// TestServer_ComfyDirect_StartHoldsWhenCold proves a cold direct request starts
// ComfyUI and serves the request, still without reaching the scheduler.
func TestServer_ComfyDirect_StartHoldsWhenCold(t *testing.T) {
	proc := &fakeDirectProcess{response: `{}`}
	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	local.notReady = false
	local.directProcess = proc // EnsureDirect returns this (a started process)
	schedulerReached := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		schedulerReached = true
	}

	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{config.ComfyUIModelID: {}}}
	s.routes()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/comfyui/history", nil))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if schedulerReached {
		t.Errorf("a direct request reached the scheduler; it must never swap")
	}
	if proc.serveCalls.Load() != 1 {
		t.Errorf("serveCalls = %d, want 1", proc.serveCalls.Load())
	}
}
