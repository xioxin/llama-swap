package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_ComfyJobs_LiveAndTerminal(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusCompleted)
	defer fc.close()

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	// Seed a tracked terminal row.
	newComfyTaskTracker(s).track(store.ActivityLogEntry{
		Timestamp: time.Now(),
		Model:     config.ComfyUIModelID,
		ReqPath:   comfyPromptPath,
	}, "done1")

	// Wait for the tracker to settle so the row carries a terminal status.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		entries := listActivity(t, s)
		if entries[0].Metadata[metaComfyStatus] == string(comfy.StatusCompleted) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/comfy/jobs", nil)
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var payload struct {
		Jobs []comfyJobView `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Jobs) != 1 {
		t.Fatalf("jobs = %d, want 1", len(payload.Jobs))
	}
	job := payload.Jobs[0]
	if job.PromptID != "done1" {
		t.Errorf("prompt_id = %q, want done1", job.PromptID)
	}
	if job.Status != string(comfy.StatusCompleted) {
		t.Errorf("status = %q, want completed", job.Status)
	}
}

// TestServer_ComfyJobs_LiveTupleJob proves the panel surfaces a live ComfyUI
// prompt from the real tuple-shaped /queue response.
func TestServer_ComfyJobs_LiveTupleJob(t *testing.T) {
	fc := newFakeComfyUpstream(comfy.StatusRunning)
	defer fc.close()
	fc.liveQueue = `{"queue_running":[[1,"live-run",{}, {}, []]],"queue_pending":[[2,"live-pend",{}, {}, []]]}`

	cfg := config.Config{
		Models: map[string]config.ModelConfig{
			config.ComfyUIModelID: {Proxy: fc.srv.URL},
		},
	}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/jobs", nil))

	var payload struct {
		Jobs []comfyJobView `json:"jobs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.Jobs) != 2 {
		t.Fatalf("jobs = %d, want 2 (live running + queued)", len(payload.Jobs))
	}
	byID := map[string]comfyJobView{}
	for _, j := range payload.Jobs {
		byID[j.PromptID] = j
	}
	if j, ok := byID["live-run"]; !ok || j.Status != "running" || !j.Live {
		t.Errorf("live-run = %+v, want running live", byID["live-run"])
	}
	if j, ok := byID["live-pend"]; !ok || j.Status != "queued" || !j.Live {
		t.Errorf("live-pend = %+v, want queued live", byID["live-pend"])
	}
}
