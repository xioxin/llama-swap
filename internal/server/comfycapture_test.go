package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

func TestServer_ComfyCaptureStoreAndRetrieve(t *testing.T) {
	mm := newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	cap := &comfy.Capture{
		PromptID:    "wf-1",
		WorkflowAPI: json.RawMessage(`{"1":{"class_type":"KSampler"}}`),
		Outputs: []comfy.Output{
			{NodeID: "47", Filename: "a.png", Type: "output", Kind: "image", Width: 8, Height: 8},
		},
	}
	if !mm.addComfyCapture(7, cap) {
		t.Fatal("addComfyCapture returned false")
	}
	got := mm.getComfyCaptureByID(7)
	if got == nil || got.PromptID != "wf-1" || len(got.Outputs) != 1 || got.Outputs[0].Filename != "a.png" {
		t.Fatalf("getComfyCaptureByID = %+v", got)
	}
	if mm.getComfyCaptureByID(999) != nil {
		t.Fatal("expected nil for unknown comfy capture")
	}
}

// TestServer_APICapture_ComfyKind proves the capture endpoint returns the Comfy
// snapshot with kind "comfy" for a ComfyUI row and the reqresp capture for a
// normal row.
func TestServer_APICapture_ComfyKind(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	defer s.store.Close()

	// metrics captures are disabled unless captureBufferMB>0; build a monitor
	// with captures on and inject directly.
	s.metrics = newTestMetricsMonitor(t, logmon.NewWriter(io.Discard), 100, 5)
	s.metrics.addComfyCapture(11, &comfy.Capture{
		PromptID: "wf-1",
		Outputs:  []comfy.Output{{Filename: "a.png", Kind: "image", Type: "output"}},
	})
	s.metrics.addCapture(ReqRespCapture{ID: 12, ReqPath: "/v1/chat/completions"})

	// Comfy row.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/captures/11", nil))
	var env struct {
		Kind    string          `json:"kind"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Kind != "comfy" {
		t.Errorf("kind = %q, want comfy", env.Kind)
	}
	if !strings.Contains(string(env.Payload), "wf-1") {
		t.Errorf("payload = %s, want prompt id", env.Payload)
	}

	// reqresp row.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/captures/12", nil))
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Kind != "reqresp" {
		t.Errorf("kind = %q, want reqresp", env.Kind)
	}
}

// TestServer_ComfyView_ProxiesAndDownload proves the media proxy forwards the
// file and honours ?download=1 with an attachment disposition.
func TestServer_ComfyView_ProxiesAndDownload(t *testing.T) {
	// Fake ComfyUI serving /api/view.
	var gotQuery string
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/view" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "4")
		_, _ = w.Write([]byte("PNG!"))
	}))
	defer fake.Close()

	cfg := config.Config{Models: map[string]config.ModelConfig{
		config.ComfyUIModelID: {Proxy: fake.URL},
	}}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	// Inline render.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/view?filename=a.png&type=output", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if w.Body.String() != "PNG!" {
		t.Errorf("body = %q, want PNG!", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(w.Header().Get("Content-Disposition"), "inline") {
		t.Errorf("disposition = %q, want inline", w.Header().Get("Content-Disposition"))
	}
	if !strings.Contains(gotQuery, "filename=a.png") {
		t.Errorf("upstream query = %q", gotQuery)
	}

	// Download.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/view?filename=a.png&type=output&download=1", nil))
	if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
		t.Errorf("disposition = %q, want attachment", w.Header().Get("Content-Disposition"))
	}
}

// TestServer_ComfyView_MissingFilename proves a request without filename is
// rejected without touching ComfyUI.
func TestServer_ComfyView_MissingFilename(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		config.ComfyUIModelID: {Proxy: "http://127.0.0.1:1"},
	}}
	s := newTestServerWithConfig(cfg, nil, nil)
	defer s.store.Close()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/view", nil))
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// TestServer_ComfyView_WakesColdComfyUI proves a preview request to a cold
// ComfyUI starts it and holds (via EnsureDirect) instead of returning 502.
func TestServer_ComfyView_WakesColdComfyUI(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNG!"))
	}))
	defer fake.Close()

	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	// ComfyUI is cold: DirectProcess reports not ready, EnsureDirect starts it.
	local.notReady = true
	local.onEnsureDirect = func() { local.notReady = false }

	cfg := config.Config{Models: map[string]config.ModelConfig{
		config.ComfyUIModelID: {Proxy: fake.URL},
	}}
	s := newTestServerWithConfig(cfg, local, nil)
	defer s.store.Close()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/view?filename=a.png&type=output", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (woke and held), body=%q", w.Code, w.Body.String())
	}
	if local.ensureDirectCalls.Load() != 1 {
		t.Errorf("EnsureDirect calls = %d, want 1 (cold start)", local.ensureDirectCalls.Load())
	}
	if w.Body.String() != "PNG!" {
		t.Errorf("body = %q", w.Body.String())
	}
}

// TestServer_ComfyView_StartFailure proves a request whose wake fails reports
// 503 rather than a misleading 502.
func TestServer_ComfyView_StartFailure(t *testing.T) {
	local := newStubRouter([]string{config.ComfyUIModelID}, "")
	local.notReady = true // EnsureDirect will fail (notReady blocks it)

	cfg := config.Config{Models: map[string]config.ModelConfig{
		config.ComfyUIModelID: {Proxy: "http://127.0.0.1:1"},
	}}
	s := newTestServerWithConfig(cfg, local, nil)
	defer s.store.Close()

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/comfy/view?filename=a.png&type=output", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", w.Code)
	}
}
