package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// Metadata keys stamped on ComfyUI activity rows so the UI can show a workflow
// task's status. They live in ActivityLogEntry.Metadata, which is already a
// free-form string map, so no schema change is needed for display.
const (
	// metaComfyPromptID records the ComfyUI prompt/job id.
	metaComfyPromptID = "comfy_prompt_id"
	// metaComfyStatus records the workflow's terminal status: completed,
	// failed, cancelled, or unknown.
	metaComfyStatus = "comfy_status"
)

// comfyPromptPath is the ComfyUI endpoint that starts a workflow. A POST here
// is a task: it is queued through the router like any mutually exclusive model
// request and then tracked to its terminal state for the activity log.
const comfyPromptPath = "/prompt"

// isComfyPromptSubmit reports whether r is a workflow submit on the ComfyUI
// endpoint (POST /prompt). The path is checked root-relative, so it matches
// both /comfyui/prompt and the dedicated comfyui.port listener.
func isComfyPromptSubmit(r *http.Request) bool {
	return r.Method == http.MethodPost && strings.TrimSuffix(r.URL.Path, "/") == comfyPromptPath
}

// isComfyPassthrough reports whether a ComfyUI request is a read-only query or
// a control call that must stay responsive even while a swap is in flight, and
// therefore bypasses the scheduler queue. Workflow submits (POST /prompt) are
// the only ComfyUI requests that queue.
//
// Reads (GET): queue/history/job/status/view/assets — polled by the open UI.
// Controls (POST): interrupt, free, job cancel — must be able to break in or
// clean up without waiting behind the very swap they influence.
func isComfyPassthrough(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if path == "" {
		path = "/"
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		// Every GET is a read: static assets, /ws, /queue, /history, /api/jobs,
		// /system_stats, /view, /object_info, etc.
		return true
	case http.MethodPost:
		switch path {
		case "/interrupt", "/free", "/history", "/queue",
			"/api/jobs/cancel":
			return true
		}
		// /api/jobs/{id}/cancel and similar per-job control paths.
		if strings.HasPrefix(path, "/api/jobs/") && strings.HasSuffix(path, "/cancel") {
			return true
		}
		return false
	default:
		return false
	}
}

// extractComfyPromptID pulls the prompt id from a ComfyUI POST /prompt response
// body: {"prompt_id":"<uuid>","number":n,"node_errors":{}}. It returns empty
// when the body has no id (validation error, non-JSON, etc.).
func extractComfyPromptID(body []byte) string {
	var resp struct {
		PromptID string `json:"prompt_id"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return ""
	}
	return resp.PromptID
}

// comfyTaskTracker records ComfyUI workflow submissions in the activity log and
// backfills each row with the workflow's terminal status. Tracking is
// asynchronous: submitting a workflow returns immediately and the row is
// updated when the prompt reaches a terminal state (or the poll deadline
// passes, in which case it is marked unknown).
type comfyTaskTracker struct {
	server *Server
}

// newComfyTaskTracker builds a tracker bound to a Server's store and ComfyUI
// client. It returns nil when the server cannot reach ComfyUI (no client), in
// which case submissions are proxied without activity tracking.
func newComfyTaskTracker(s *Server) *comfyTaskTracker {
	return &comfyTaskTracker{server: s}
}

// track records a submitted workflow and starts polling for its terminal
// state. row carries the submit-time activity fields (timestamp, source, path,
// response status). promptID is the ComfyUI prompt id from the response.
func (t *comfyTaskTracker) track(row store.ActivityLogEntry, promptID string) {
	if t == nil || promptID == "" {
		return
	}
	if row.Metadata == nil {
		row.Metadata = map[string]string{}
	}
	row.Metadata[metaComfyPromptID] = promptID
	// Terminal status is unknown until the workflow finishes.
	row.Metadata[metaComfyStatus] = string(comfy.StatusRunning)

	stored, ok := t.server.metrics.queueMetrics(row)
	if !ok {
		return
	}
	t.server.metrics.emitMetric(stored)

	go t.awaitTerminal(stored, promptID)
}

// awaitTerminal polls ComfyUI until promptID reaches a terminal state and then
// updates the activity row. It gives up after the configured tracking window
// and records "unknown" so the row never stays stuck as running. The poll uses
// a detached context: a cancelled client request must not abandon the row.
func (t *comfyTaskTracker) awaitTerminal(row store.ActivityLogEntry, promptID string) {
	client := t.server.comfyClient()
	if client == nil {
		return
	}

	deadline := time.Now().Add(t.server.comfyTrackWindow())
	ctx, cancel := context.WithTimeout(context.Background(), t.server.comfyTrackWindow())
	defer cancel()

	for {
		status, err := client.JobState(ctx, promptID)
		if err == nil && status.IsTerminal() {
			t.finalize(row, promptID, status)
			return
		}
		if time.Now().After(deadline) {
			t.finalize(row, promptID, comfy.StatusUnknown)
			return
		}
		select {
		case <-ctx.Done():
			t.finalize(row, promptID, comfy.StatusUnknown)
			return
		case <-time.After(t.server.comfyPollInterval()):
		}
	}
}

// finalize stamps the terminal status and elapsed duration on the activity row
// and persists the update.
func (t *comfyTaskTracker) finalize(row store.ActivityLogEntry, promptID string, status comfy.JobStatus) {
	if row.Metadata == nil {
		row.Metadata = map[string]string{}
	}
	row.Metadata[metaComfyPromptID] = promptID
	row.Metadata[metaComfyStatus] = string(status)
	row.DurationMs = int(time.Since(row.Timestamp).Milliseconds())
	if status == comfy.StatusUnknown && row.ErrorMsg == "" {
		row.ErrorMsg = "comfyui workflow status not observed before timeout"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := t.server.store.Activity().Update(ctx, row); err != nil {
		t.server.logs.ProxyLogs.Warnf("comfyui: updating activity row %d: %v", row.ID, err)
		return
	}
	t.server.metrics.emitMetric(row)
}

// comfyTrackWindow returns how long a workflow is polled before its row is
// marked unknown. Video and other long renders can exceed this; the row still
// records the submission and elapsed time. Bounded so a lost prompt cannot pin
// a goroutine forever.
func (s *Server) comfyTrackWindow() time.Duration {
	return 2 * time.Hour
}

// comfyPollInterval is the delay between terminal-state probes.
func (s *Server) comfyPollInterval() time.Duration {
	return 2 * time.Second
}

// comfyClient returns the router-independent ComfyUI client used for task
// tracking, built from the comfyui_auto model's proxy. It is nil when the model
// or its proxy is not configured, in which case tracking is disabled. The lazy
// build is guarded against concurrent submits.
func (s *Server) comfyClient() *comfy.Client {
	m, ok := s.cfg.Models[config.ComfyUIModelID]
	if !ok || m.Proxy == "" {
		return nil
	}
	s.comfyMu.Lock()
	defer s.comfyMu.Unlock()
	if s.comfy == nil {
		s.comfy = comfy.New(m.Proxy, 10*time.Second)
	}
	return s.comfy
}

// wrapComfySubmit tees the ComfyUI submit response so the prompt id can be
// read after the handler returns, without disturbing the client. Only used for
// POST /prompt.
func wrapComfySubmit(w http.ResponseWriter) *responseBodyCopier {
	return newBodyCopier(w)
}

// recordComfySubmit builds the activity row for a submitted workflow and starts
// terminal tracking. It is called after the submit response has been written.
func recordComfySubmit(s *Server, r *http.Request, recorder *responseBodyCopier) {
	promptID := extractComfyPromptID(recorder.body.Bytes())
	if promptID == "" {
		return
	}
	row := store.ActivityLogEntry{
		Timestamp:       recorder.StartTime(),
		Model:           config.ComfyUIModelID,
		ReqPath:         comfyPromptPath,
		RespContentType: recorder.Header().Get("Content-Type"),
		RespStatusCode:  recorder.Status(),
		Src:             activitySource(r),
	}
	if data, ok := swaputil.ReadContext(r.Context()); ok && len(data.Metadata) > 0 {
		row.Metadata = make(map[string]string, len(data.Metadata))
		for k, v := range data.Metadata {
			row.Metadata[k] = v
		}
	}
	newComfyTaskTracker(s).track(row, promptID)
}
