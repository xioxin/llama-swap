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
	// vanish models a task cancelled while queued: it is in the queue at first,
	// then disappears from both the queue and history.
	vanish atomic.Bool
	// holdsID is the prompt id the queue reports while not vanished.
	holdsID string
}

func newFakeComfyUpstream(status comfy.JobStatus) *fakeComfyUpstream {
	f := &fakeComfyUpstream{status: status}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		if f.liveQueue != "" {
			_, _ = w.Write([]byte(f.liveQueue))
			return
		}
		if f.holdsID != "" && !f.vanish.Load() {
			_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[[1,"` + f.holdsID + `",{}, {}, []]]}`))
			return
		}
		// Never in the queue: terminal state is read from /history.
		_, _ = w.Write([]byte(`{"queue_running":[],"queue_pending":[]}`))
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/history/")
		if f.vanish.Load() {
			// Cancelled before it ran: no history record at all.
			_, _ = w.Write([]byte(`{}`))
			return
		}
		msg := `[]`
		switch f.status {
		case comfy.StatusFailed:
			msg = `[["execution_error",{}]]`
		case comfy.StatusCancelled:
			msg = `[["execution_interrupted",{}]]`
		}
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
		// ComfyUI registers /api/prompt too, and its web frontend submits here.
		{http.MethodPost, "/api/prompt", true},
		{http.MethodPost, "/api/prompt/", true},
		{http.MethodGet, "/prompt", false},
		{http.MethodGet, "/api/prompt", false},
		{http.MethodPost, "/history", false},
		{http.MethodPost, "/interrupt", false},
		{http.MethodPost, "/api/settings/Comfy.Queue.History.Expanded", false},
		{http.MethodPost, "/api/prompts", false},
		{http.MethodPost, "/api/prompt/extra", false},
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
	// Only the workflow submits are swappable work: both /prompt and the
	// frontend's /api/prompt must swap, not pass through.
	for _, path := range []string{"/prompt", "/api/prompt"} {
		submit := httptest.NewRequest(http.MethodPost, path, nil)
		if isComfyDirect(submit) {
			t.Errorf("isComfyDirect(POST %s) = true, want false (it is a swapper)", path)
		}
		if !isComfyPromptSubmit(submit) {
			t.Errorf("isComfyPromptSubmit(POST %s) = false, want true", path)
		}
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

// TestServer_ComfyPrompt_SwapsViaScheduler is the regression guard for "the
// frontend POST /api/prompt did not swap the LLM out". Both /prompt and
// /api/prompt must reach the scheduler (the only swapper) and record a task
// row with the extracted prompt id.
func TestServer_ComfyPrompt_SwapsViaScheduler(t *testing.T) {
	for _, path := range []string{"/comfyui/prompt", "/comfyui/api/prompt"} {
		local := newStubRouter([]string{config.ComfyUIModelID}, "")
		local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"prompt_id":"wf-1","number":1,"node_errors":{}}`))
		}
		// A direct-process hit would mean no swap; it must NOT be used here.
		proc := &fakeDirectProcess{response: `{}`}
		local.directProcess = proc

		s := newTestServerWithConfig(config.Config{
			Models: map[string]config.ModelConfig{config.ComfyUIModelID: {}},
		}, local, newStubRouter(nil, ""))
		defer s.store.Close()

		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, w.Code)
		}
		if proc.serveCalls.Load() != 0 {
			t.Errorf("%s used DirectProcess (%d calls); a submit must go through the scheduler", path, proc.serveCalls.Load())
		}
		entries := listActivity(t, s)
		if len(entries) != 1 {
			t.Fatalf("%s activity rows = %d, want 1", path, len(entries))
		}
		if entries[0].Metadata[metaComfyPromptID] != "wf-1" {
			t.Errorf("%s prompt id = %q, want wf-1", path, entries[0].Metadata[metaComfyPromptID])
		}
	}
}

// TestComfyTask_CancelledWhileQueued proves a task that ComfyUI drops from its
// queue before starting (cancelled in the ComfyUI UI) is finalised as
// cancelled, not left stuck as running.
func TestComfyTask_CancelledWhileQueued(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusCompleted)
	defer fc.close()
	fc.holdsID = "q1"      // first seen in the pending queue
	fc.vanish.Store(false) // ... then it disappears from queue and history

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	// Speed up the poll so the test does not wait a full interval per tick.
	tracker := newComfyTaskTracker(s)
	row := store.ActivityLogEntry{Timestamp: time.Now(), Model: config.ComfyUIModelID, ReqPath: comfyPromptPath}
	tracker.track(row, "q1")

	// Let the tracker observe the prompt in the queue at least once.
	time.Sleep(50 * time.Millisecond)
	fc.vanish.Store(true)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		entries := listActivity(t, s)
		if entries[0].Metadata[metaComfyStatus] == string(comfy.StatusCancelled) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("status never resolved; got %q, want cancelled", listActivity(t, s)[0].Metadata[metaComfyStatus])
}

// TestComfyTask_NotYetRegisteredIsNotCancelled proves a prompt ComfyUI has not
// yet put in its queue is not prematurely marked cancelled while it is still
// being registered.
func TestComfyTask_NotYetRegisteredIsNotCancelled(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusCompleted)
	defer fc.close()
	// Queue empty and no history from the very first poll: prompt never seen.
	fc.vanish.Store(true)

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	tracker := newComfyTaskTracker(s)
	tracker.track(store.ActivityLogEntry{Timestamp: time.Now(), Model: config.ComfyUIModelID, ReqPath: comfyPromptPath}, "never-seen")

	// It must remain running (awaiting), not flip to cancelled.
	time.Sleep(300 * time.Millisecond)
	entries := listActivity(t, s)
	if entries[0].Metadata[metaComfyStatus] != string(comfy.StatusRunning) {
		t.Errorf("status = %q, want running (still awaiting registration)", entries[0].Metadata[metaComfyStatus])
	}
}
