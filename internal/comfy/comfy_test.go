package comfy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeComfy is a programmable ComfyUI stub. Each endpoint's handler can be set
// per test; unset endpoints return empty-but-valid JSON.
type fakeComfy struct {
	mux        *http.ServeMux
	queueJSON  string
	statsJSON  string
	histJSON   string
	freeCalls  int
	freeStatus int
	srv        *httptest.Server
}

func newFakeComfy() *fakeComfy {
	f := &fakeComfy{
		queueJSON:  `{"queue_running":[],"queue_pending":[]}`,
		statsJSON:  `{"devices":[{"vram_total":100,"vram_free":100}]}`,
		histJSON:   `{}`,
		freeStatus: 200,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.queueJSON))
	})
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.statsJSON))
	})
	mux.HandleFunc("/free", func(w http.ResponseWriter, r *http.Request) {
		f.freeCalls++
		w.WriteHeader(f.freeStatus)
	})
	mux.HandleFunc("/history/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.histJSON))
	})
	f.mux = mux
	f.srv = httptest.NewServer(mux)
	return f
}

func (f *fakeComfy) close() { f.srv.Close() }

func (f *fakeComfy) client() *Client { return New(f.srv.URL, 2*time.Second) }

func TestComfy_IdleState_Empty(t *testing.T) {
	f := newFakeComfy()
	defer f.close()

	st, err := f.client().IdleState(context.Background())
	if err != nil {
		t.Fatalf("IdleState: %v", err)
	}
	if !st.QueueEmpty {
		t.Errorf("QueueEmpty = false, want true")
	}
	if st.VramUsedFraction != 0 {
		t.Errorf("VramUsedFraction = %v, want 0", st.VramUsedFraction)
	}
	if !st.IsIdle(0.02) {
		t.Errorf("IsIdle(0.02) = false, want true")
	}
}

func TestComfy_IdleState_RunningNotIdle(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[{"prompt_id":"a"}],"queue_pending":[]}`
	// 40 of 100 in use -> busy.
	f.statsJSON = `{"devices":[{"vram_total":100,"vram_free":60}]}`

	st, err := f.client().IdleState(context.Background())
	if err != nil {
		t.Fatalf("IdleState: %v", err)
	}
	if st.QueueEmpty {
		t.Errorf("QueueEmpty = true, want false (running job)")
	}
	if st.IsIdle(0.02) {
		t.Errorf("IsIdle = true, want false (running job)")
	}
}

func TestComfy_IdleState_VramNotIdle(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// Empty queue but VRAM still held -> not idle under a small threshold.
	f.statsJSON = `{"devices":[{"vram_total":100,"vram_free":50}]}`

	st, err := f.client().IdleState(context.Background())
	if err != nil {
		t.Fatalf("IdleState: %v", err)
	}
	if !st.QueueEmpty {
		t.Errorf("QueueEmpty = false, want true")
	}
	if st.IsIdle(0.02) {
		t.Errorf("IsIdle(0.02) = true, want false (VRAM held)")
	}
	if !st.IsIdle(0.6) {
		t.Errorf("IsIdle(0.6) = false, want true (threshold tolerates it)")
	}
}

// The resident CUDA context after a free is ~30-500MiB on a 33GiB card (~1%).
// A small used fraction with an empty queue must still count as idle at the
// default threshold. This mirrors the measurements from the ComfyUI host.
func TestComfy_IdleState_ResidualContextIsIdle(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// 796MiB resident of 33670758400 total -> ~2.4%, but with a tolerant
	// threshold (default 2% is strict); model the ~30MiB case (~0.09%).
	f.statsJSON = `{"devices":[{"vram_total":33670758400,"vram_free":33639301222}]}`

	st, err := f.client().IdleState(context.Background())
	if err != nil {
		t.Fatalf("IdleState: %v", err)
	}
	if !st.IsIdle(0.02) {
		t.Errorf("IsIdle(0.02) = false, want true (residual context only, used=%v)", st.VramUsedFraction)
	}
}

func TestComfy_IdleState_AggregatesDevices(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.statsJSON = `{"devices":[
		{"vram_total":100,"vram_free":100},
		{"vram_total":100,"vram_free":100}
	]}`

	st, err := f.client().IdleState(context.Background())
	if err != nil {
		t.Fatalf("IdleState: %v", err)
	}
	if st.VramUsedFraction != 0 {
		t.Errorf("VramUsedFraction = %v, want 0 across devices", st.VramUsedFraction)
	}
}

func TestComfy_IdleState_ProbeErrorIsBusy(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// Break /queue so the probe errors; the state must be conservative.
	f.mux = http.NewServeMux()
	f.srv.Close()
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	f2 := New(f.srv.URL, 2*time.Second)

	st, err := f2.IdleState(context.Background())
	if err == nil {
		t.Fatalf("IdleState error = nil, want error on failing probe")
	}
	if st.QueueEmpty {
		t.Errorf("QueueEmpty = true, want false on probe error")
	}
	if st.VramUsedFraction != 1 {
		t.Errorf("VramUsedFraction = %v, want 1 on probe error", st.VramUsedFraction)
	}
}

func TestComfy_FreeMemory_SendsUnloadAndFree(t *testing.T) {
	f := newFakeComfy()
	defer f.close()

	if err := f.client().FreeMemory(context.Background()); err != nil {
		t.Fatalf("FreeMemory: %v", err)
	}
	if f.freeCalls != 1 {
		t.Errorf("freeCalls = %d, want 1", f.freeCalls)
	}
}

func TestComfy_FreeMemory_Error(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.freeStatus = 500

	if err := f.client().FreeMemory(context.Background()); err == nil {
		t.Fatalf("FreeMemory error = nil, want error on 500")
	}
}

func TestComfy_WaitIdle_ReturnsWhenIdle(t *testing.T) {
	f := newFakeComfy()
	defer f.close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.client().WaitIdle(ctx, 0.02, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
}

func TestComfy_WaitIdle_TimesOutWhenBusy(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[{"prompt_id":"a"}],"queue_pending":[]}`

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.client().WaitIdle(ctx, 0.02, 5*time.Millisecond); err == nil {
		t.Fatalf("WaitIdle error = nil, want timeout while busy")
	}
}

func TestComfy_WaitIdle_BecomesIdle(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// Busy first, then idle after the first probe. Swap the handlers once.
	f.queueJSON = `{"queue_running":[{"prompt_id":"a"}],"queue_pending":[]}`
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.queueJSON = `{"queue_running":[],"queue_pending":[]}`
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.client().WaitIdle(ctx, 0.02, 5*time.Millisecond); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
}

func TestComfy_WaitQueueEmpty_IgnoresVram(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// Queue empty but VRAM still held: WaitQueueEmpty must return (VRAM is not
	// its concern), unlike WaitIdle.
	f.statsJSON = `{"devices":[{"vram_total":100,"vram_free":10}]}`

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.client().WaitQueueEmpty(ctx, 5*time.Millisecond); err != nil {
		t.Fatalf("WaitQueueEmpty: %v", err)
	}
}

func TestComfy_WaitQueueEmpty_WaitsForWorkflow(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[{"prompt_id":"a"}],"queue_pending":[]}`

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.client().WaitQueueEmpty(ctx, 5*time.Millisecond); err == nil {
		t.Fatalf("WaitQueueEmpty error = nil, want timeout while a workflow runs")
	}
}

func TestComfy_WaitVramReleased_ConfirmsFree(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	// VRAM still held -> not released; a short confirm times out.
	f.statsJSON = `{"devices":[{"vram_total":100,"vram_free":10}]}`

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := f.client().WaitVramReleased(ctx, 0.02, 5*time.Millisecond); err == nil {
		t.Fatalf("WaitVramReleased error = nil, want timeout while VRAM held")
	}
}

func TestComfy_JobState_Running(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[{"prompt_id":"abc"}],"queue_pending":[]}`

	got, err := f.client().JobState(context.Background(), "abc")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusRunning {
		t.Errorf("JobState = %q, want running", got)
	}
}

func TestComfy_JobState_Queued(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[],"queue_pending":[{"prompt_id":"abc"}]}`

	got, err := f.client().JobState(context.Background(), "abc")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusQueued {
		t.Errorf("JobState = %q, want queued", got)
	}
}

func TestComfy_Jobs_ListsLiveWork(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[{"prompt_id":"r1"}],"queue_pending":[{"prompt_id":"p1"},{"prompt_id":"p2"}]}`

	jobs, err := f.client().Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("Jobs = %d, want 3", len(jobs))
	}
	byID := map[string]JobStatus{}
	for _, j := range jobs {
		byID[j.PromptID] = j.Status
	}
	if byID["r1"] != StatusRunning {
		t.Errorf("r1 = %q, want running", byID["r1"])
	}
	if byID["p1"] != StatusQueued || byID["p2"] != StatusQueued {
		t.Errorf("pending = %q,%q, want queued,queued", byID["p1"], byID["p2"])
	}
}

func TestComfy_JobState_Completed(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.histJSON = `{"abc":{"status":{"completed":true,"status_str":"success","messages":[]}}}`

	got, err := f.client().JobState(context.Background(), "abc")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusCompleted {
		t.Errorf("JobState = %q, want completed", got)
	}
}

func TestComfy_JobState_Failed(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.histJSON = `{"abc":{"status":{"completed":true,"status_str":"success","messages":[["execution_error",{}]]}}}`

	got, err := f.client().JobState(context.Background(), "abc")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusFailed {
		t.Errorf("JobState = %q, want failed", got)
	}
}

func TestComfy_JobState_Cancelled(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.histJSON = `{"abc":{"status":{"completed":true,"status_str":"success","messages":[["execution_interrupted",{}]]}}}`

	got, err := f.client().JobState(context.Background(), "abc")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusCancelled {
		t.Errorf("JobState = %q, want cancelled", got)
	}
}

func TestComfy_JobState_Unknown(t *testing.T) {
	f := newFakeComfy()
	defer f.close()

	got, err := f.client().JobState(context.Background(), "nope")
	if err != nil {
		t.Fatalf("JobState: %v", err)
	}
	if got != StatusUnknown {
		t.Errorf("JobState = %q, want unknown", got)
	}
}

func TestComfy_JobStatus_IsTerminal(t *testing.T) {
	cases := map[JobStatus]bool{
		StatusCompleted: true,
		StatusFailed:    true,
		StatusCancelled: true,
		StatusRunning:   false,
		StatusUnknown:   false,
	}
	for st, want := range cases {
		if got := st.IsTerminal(); got != want {
			t.Errorf("IsTerminal(%q) = %v, want %v", st, got, want)
		}
	}
}

// TestComfy_QueueTupleShape locks the real ComfyUI /queue format: queue items
// are tuples (number, prompt_id, prompt, extra_data, outputs), so the prompt id
// is at index 1 — not a "prompt_id" object field.
func TestComfy_QueueTupleShape(t *testing.T) {
	f := newFakeComfy()
	defer f.close()
	f.queueJSON = `{"queue_running":[[1,"run-1",{}, {}, []]],"queue_pending":[[2,"pend-1",{}, {}, []],[3,"pend-2",{}, {}, []]]}`

	jobs, err := f.client().Jobs(context.Background())
	if err != nil {
		t.Fatalf("Jobs: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("Jobs = %d, want 3", len(jobs))
	}
	byID := map[string]JobStatus{}
	for _, j := range jobs {
		byID[j.PromptID] = j.Status
	}
	if byID["run-1"] != StatusRunning {
		t.Errorf("run-1 = %q, want running", byID["run-1"])
	}
	if byID["pend-1"] != StatusQueued || byID["pend-2"] != StatusQueued {
		t.Errorf("pending = %q,%q, want queued,queued", byID["pend-1"], byID["pend-2"])
	}

	// JobState must also see the tuple ids.
	if got, err := f.client().JobState(context.Background(), "run-1"); err != nil || got != StatusRunning {
		t.Errorf("JobState(run-1) = %q,%v, want running", got, err)
	}
	if got, err := f.client().JobState(context.Background(), "pend-2"); err != nil || got != StatusQueued {
		t.Errorf("JobState(pend-2) = %q,%v, want queued", got, err)
	}
}

func TestComfy_PromptIDFromQueueItem(t *testing.T) {
	cases := map[string]string{
		`[1,"abc",{}, {}, []]`: "abc",
		`{"prompt_id":"xyz"}`:  "xyz",
		`[1]`:                  "",
		`"garbage"`:            "",
	}
	for raw, want := range cases {
		if got := promptIDFromQueueItem(json.RawMessage(raw)); got != want {
			t.Errorf("promptIDFromQueueItem(%s) = %q, want %q", raw, got, want)
		}
	}
}
