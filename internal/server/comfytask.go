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

// comfyPromptPath is the canonical ComfyUI submit path, used as the default
// task-row ReqPath when a real request path is not being recorded.
const comfyPromptPath = "/prompt"

// comfyViewProxyPath is the llama-swap-relative base that streams ComfyUI output
// files from the running ComfyUI process. Capture snapshots stamp this on each
// output so the UI loads media from the same origin.
const comfyViewProxyPath = "/api/comfy/view"

// comfyPromptPaths are the ComfyUI endpoints that start a workflow. ComfyUI
// registers every route twice — once bare and once under the /api prefix (see
// ComfyUI server.py add_routes) — and its web frontend submits to /api/prompt
// while CLI/API clients use /prompt. Both are the same handler and must be
// recognised as a task. A POST to either is queued through the router like any
// mutually exclusive model request and tracked to its terminal state.
var comfyPromptPaths = map[string]bool{
	"/prompt":     true,
	"/api/prompt": true,
}

// isComfyPromptSubmit reports whether r is a workflow submit on the ComfyUI
// endpoint. Only exact POST /prompt or POST /api/prompt (optional trailing
// slash) count; the path is root-relative, so it matches both /comfyui/... and
// the dedicated comfyui.port listener. A GET on either path is ComfyUI's queue
// info query, not a submit.
func isComfyPromptSubmit(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	return comfyPromptPaths[path]
}

// isComfyDirect reports whether a ComfyUI request must be served straight to a
// ready ComfyUI process and must NEVER enter the model scheduler — i.e. it can
// neither queue for the GPU nor trigger a swap. Only a workflow submit
// (POST /prompt or /api/prompt) is allowed to swap; every other request (reads,
// settings writes, uploads, interrupt, free, job cancel, static assets) is
// direct.
//
// This is what keeps the open ComfyUI UI from unloading a running LLM: a
// history panel poll or a settings POST is not a reason to give ComfyUI the
// GPU, so it must not reach EvictionFor.
func isComfyDirect(r *http.Request) bool {
	return !isComfyPromptSubmit(r)
}

// comfySubmitPath returns the normalised ComfyUI submit path recorded on an
// activity/task row (e.g. "/api/prompt" or "/prompt").
func comfySubmitPath(r *http.Request) string {
	path := strings.TrimSuffix(r.URL.Path, "/")
	if comfyPromptPaths[path] {
		return path
	}
	return comfyPromptPath
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

	// seenLive records that ComfyUI has acknowledged the prompt (it appeared in
	// the running/pending queue). A prompt that later vanishes from both the
	// queue and history was cancelled before it ever ran — ComfyUI removes such
	// an item on cancel and never records it in history. We only conclude
	// "cancelled" after it was once seen, so a submit that ComfyUI has not yet
	// registered is not falsely marked cancelled.
	seenLive := false

	for {
		status, err := client.JobState(ctx, promptID)
		if err == nil {
			if status.IsTerminal() {
				t.finalize(row, promptID, status)
				return
			}
			if status == comfy.StatusQueued || status == comfy.StatusRunning {
				seenLive = true
			} else if seenLive {
				// Was live, now neither queued nor in history: cancelled before
				// start.
				t.finalize(row, promptID, comfy.StatusCancelled)
				return
			}
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

	// Snapshot the finished workflow (workflow graphs, outputs, timing, node
	// and model list) into the capture store so the Capture view stays usable
	// after ComfyUI restarts or clears its history. Output media is not stored;
	// it is proxied from ComfyUI on demand.
	if status.IsTerminal() {
		if snap := t.snapshot(ctx, promptID); snap != nil {
			t.server.metrics.addComfyCapture(row.ID, snap)
		}
	}

	if err := t.server.store.Activity().Update(ctx, row); err != nil {
		t.server.logs.ProxyLogs.Warnf("comfyui: updating activity row %d: %v", row.ID, err)
		return
	}
	t.server.metrics.emitMetric(row)
}

// snapshot fetches and parses the ComfyUI history record for a prompt and
// stamps proxy URLs on its outputs. Returns nil when ComfyUI is unreachable or
// the record is gone — the activity row is still updated without a capture.
func (t *comfyTaskTracker) snapshot(ctx context.Context, promptID string) *comfy.Capture {
	client := t.server.comfyClient()
	if client == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cap, err := client.History(cctx, promptID)
	if err != nil {
		t.server.logs.ProxyLogs.Debugf("comfyui: capture snapshot for %s unavailable: %v", promptID, err)
		return nil
	}
	cap.SetProxyURLs(comfyViewProxyPath)
	return cap
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
		ReqPath:         comfySubmitPath(r),
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
