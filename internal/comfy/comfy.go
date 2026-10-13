// Package comfy is a small HTTP client for the ComfyUI server API. It is used
// by the router to decide when ComfyUI is idle (safe to hand the GPU to another
// model) and to release its GPU memory without stopping the process.
//
// The endpoints used are the plain ComfyUI API, no custom nodes required:
//
//	GET  /queue            -> queue_running + queue_pending (what is executing)
//	GET  /system_stats     -> per-device vram_total/vram_free (memory in use)
//	POST /free             -> {"unload_models":true,"free_memory":true}
//	GET  /history/{id}     -> per-prompt terminal status (for activity rows)
package comfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// JobStatus is the terminal state of a ComfyUI prompt as reported by history.
type JobStatus string

const (
	// StatusQueued means the prompt is pending in ComfyUI's queue, not yet
	// executing.
	StatusQueued JobStatus = "queued"
	// StatusRunning means the prompt is currently executing.
	StatusRunning JobStatus = "running"
	// StatusCompleted means the prompt finished successfully.
	StatusCompleted JobStatus = "completed"
	// StatusFailed means the prompt errored.
	StatusFailed JobStatus = "failed"
	// StatusCancelled means the prompt was interrupted or dequeued.
	StatusCancelled JobStatus = "cancelled"
	// StatusUnknown means the prompt is not in history and not in the queue.
	StatusUnknown JobStatus = "unknown"
)

// IsTerminal reports whether the status is a finished state.
func (s JobStatus) IsTerminal() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// IsPending reports whether the prompt still has work queued or executing.
func (s JobStatus) IsPending() bool {
	return s == StatusQueued || s == StatusRunning
}

// IdleState is one snapshot of whether ComfyUI is using the GPU.
type IdleState struct {
	// QueueEmpty reports whether nothing is running or pending in ComfyUI's
	// own prompt queue (independent of any llama-swap-side queue).
	QueueEmpty bool
	// VramUsedFraction is the fraction of total device VRAM in use across
	// all reported devices, in 0..1. It includes the resident CUDA context
	// that remains after a memory free.
	VramUsedFraction float64
}

// IsIdle reports whether ComfyUI holds no work and its VRAM usage is within
// the given free fraction threshold. vramFreeFraction is the fraction of VRAM
// that may still be in use and still count as idle (see
// config.DefaultComfyUIVramFreeFraction).
func (s IdleState) IsIdle(vramFreeFraction float64) bool {
	return s.QueueEmpty && s.VramUsedFraction <= vramFreeFraction
}

// Client talks to one ComfyUI server at base (scheme://host:port, no trailing
// slash). base is the resolved proxy address of the comfyui_auto model.
type Client struct {
	base string
	http *http.Client
}

// New builds a Client for the ComfyUI server at base. timeout bounds each
// individual API call; use a short timeout for probes and a longer one for the
// free call. A timeout <= 0 falls back to 5s.
func New(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{
		base: base,
		http: &http.Client{Timeout: timeout},
	}
}

// getJSON performs a GET and decodes the JSON body into out. Non-2xx responses
// are errors carrying the status and a short body excerpt.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return fmt.Errorf("comfy: building GET %s: %w", path, err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("comfy: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("comfy: GET %s: status %d: %s", path, resp.StatusCode, bytes.TrimSpace(body))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("comfy: GET %s: decoding body: %w", path, err)
	}
	return nil
}

// IdleState probes ComfyUI once and returns whether it is using the GPU. The
// two signals are read independently so a partially failing probe still yields
// useful information: a failed queue read reports QueueEmpty=false (not idle),
// and a failed stats read leaves VramUsedFraction at 1 (busy) rather than 0.
func (c *Client) IdleState(ctx context.Context) (IdleState, error) {
	var st IdleState
	var errs []error

	if err := c.probeQueue(ctx, &st); err != nil {
		errs = append(errs, err)
	}
	if err := c.probeVram(ctx, &st); err != nil {
		errs = append(errs, err)
	}

	if len(errs) > 0 {
		return st, errorsJoin(errs...)
	}
	return st, nil
}

// probeQueue fills st.QueueEmpty from GET /queue. On error it reports not
// empty (conservative: unknown work must not be treated as idle).
func (c *Client) probeQueue(ctx context.Context, st *IdleState) error {
	var q struct {
		QueueRunning []json.RawMessage `json:"queue_running"`
		QueuePending []json.RawMessage `json:"queue_pending"`
	}
	if err := c.getJSON(ctx, "/queue", &q); err != nil {
		st.QueueEmpty = false
		return err
	}
	st.QueueEmpty = len(q.QueueRunning) == 0 && len(q.QueuePending) == 0
	return nil
}

// probeVram fills st.VramUsedFraction from GET /system_stats. On error it
// reports a fully used device (conservative).
func (c *Client) probeVram(ctx context.Context, st *IdleState) error {
	st.VramUsedFraction = 1
	var sys struct {
		Devices []struct {
			VramTotal int64 `json:"vram_total"`
			VramFree  int64 `json:"vram_free"`
		} `json:"devices"`
	}
	if err := c.getJSON(ctx, "/system_stats", &sys); err != nil {
		return err
	}
	if len(sys.Devices) == 0 {
		// No device reported (e.g. CPU-only). Nothing occupies a GPU.
		st.VramUsedFraction = 0
		return nil
	}
	var total, free int64
	for _, d := range sys.Devices {
		total += d.VramTotal
		free += d.VramFree
	}
	if total <= 0 {
		st.VramUsedFraction = 1
		return nil
	}
	used := total - free
	if used < 0 {
		used = 0
	}
	st.VramUsedFraction = float64(used) / float64(total)
	return nil
}

// FreeMemory tells ComfyUI to unload its models and free its caches. It blocks
// until ComfyUI acknowledges the request. ComfyUI applies the free on its
// worker loop, so the response returns before the memory is fully released;
// callers that need the release to have taken effect should re-check IdleState.
func (c *Client) FreeMemory(ctx context.Context) error {
	payload := []byte(`{"unload_models":true,"free_memory":true}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/free", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("comfy: building POST /free: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("comfy: POST /free: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("comfy: POST /free: status %d: %s", resp.StatusCode, bytes.TrimSpace(body))
	}
	return nil
}

// WaitIdle polls IdleState until it is idle within vramFreeFraction, the poll
// interval elapses between attempts, or ctx is done. It returns nil on idle and
// a context error on timeout/cancel. A failed probe resets the loop (not idle).
// The first probe runs immediately; poll <= 0 falls back to 200ms.
func (c *Client) WaitIdle(ctx context.Context, vramFreeFraction float64, poll time.Duration) error {
	return c.wait(ctx, poll, func(st IdleState) bool { return st.IsIdle(vramFreeFraction) })
}

// WaitQueueEmpty blocks until ComfyUI has no running or pending workflow of its
// own — the safe point to hand the GPU to another model. It ignores VRAM: a
// finished workflow may still hold memory until a /free. poll <= 0 falls back
// to 200ms. A failed probe resets the loop (treated as still busy).
func (c *Client) WaitQueueEmpty(ctx context.Context, poll time.Duration) error {
	return c.wait(ctx, poll, func(st IdleState) bool { return st.QueueEmpty })
}

// WaitVramReleased blocks until ComfyUI's VRAM usage drops to within
// vramFreeFraction, i.e. the /free has taken effect. It is the post-free
// confirmation, not a gate: callers give it a short bound and proceed on
// timeout. poll <= 0 falls back to 200ms.
func (c *Client) WaitVramReleased(ctx context.Context, vramFreeFraction float64, poll time.Duration) error {
	return c.wait(ctx, poll, func(st IdleState) bool { return st.VramUsedFraction <= vramFreeFraction })
}

// wait polls IdleState until done reports true. The first probe runs
// immediately; poll <= 0 falls back to 200ms. A failed probe resets the loop.
func (c *Client) wait(ctx context.Context, poll time.Duration, done func(IdleState) bool) error {
	if poll <= 0 {
		poll = 200 * time.Millisecond
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		if st, err := c.IdleState(ctx); err == nil && done(st) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// JobState reports the current state of a prompt by id. It first checks the
// running/pending queue and then history. A prompt in neither place is
// StatusUnknown. Terminal history states map to completed/failed/cancelled.
func (c *Client) JobState(ctx context.Context, promptID string) (JobStatus, error) {
	if promptID == "" {
		return StatusUnknown, fmt.Errorf("comfy: empty prompt id")
	}

	// Still queued or executing?
	var q struct {
		QueueRunning []struct {
			PromptID string `json:"prompt_id"`
		} `json:"queue_running"`
		QueuePending []struct {
			PromptID string `json:"prompt_id"`
		} `json:"queue_pending"`
	}
	if err := c.getJSON(ctx, "/queue", &q); err == nil {
		for _, item := range q.QueueRunning {
			if item.PromptID == promptID {
				return StatusRunning, nil
			}
		}
		for _, item := range q.QueuePending {
			if item.PromptID == promptID {
				return StatusQueued, nil
			}
		}
	}

	// Terminal state in history? The history entry carries a status object
	// whose completed flag and messages describe the outcome.
	var hist map[string]struct {
		Status struct {
			Completed bool    `json:"completed"`
			Messages  [][]any `json:"messages"`
			StatusStr string  `json:"status_str"`
		} `json:"status"`
	}
	if err := c.getJSON(ctx, "/history/"+promptID, &hist); err != nil {
		return StatusUnknown, err
	}
	entry, ok := hist[promptID]
	if !ok {
		return StatusUnknown, nil
	}
	return classifyHistory(entry.Status.Completed, entry.Status.StatusStr, entry.Status.Messages), nil
}

// Job is one live ComfyUI prompt still queued or executing.
type Job struct {
	PromptID string
	Status   JobStatus // StatusQueued or StatusRunning
}

// Jobs lists the prompts ComfyUI currently has queued or running. It is the
// live-work view used for task visibility; terminal prompts are not included.
func (c *Client) Jobs(ctx context.Context) ([]Job, error) {
	var q struct {
		QueueRunning []struct {
			PromptID string `json:"prompt_id"`
		} `json:"queue_running"`
		QueuePending []struct {
			PromptID string `json:"prompt_id"`
		} `json:"queue_pending"`
	}
	if err := c.getJSON(ctx, "/queue", &q); err != nil {
		return nil, err
	}
	jobs := make([]Job, 0, len(q.QueueRunning)+len(q.QueuePending))
	for _, item := range q.QueueRunning {
		if item.PromptID != "" {
			jobs = append(jobs, Job{PromptID: item.PromptID, Status: StatusRunning})
		}
	}
	for _, item := range q.QueuePending {
		if item.PromptID != "" {
			jobs = append(jobs, Job{PromptID: item.PromptID, Status: StatusQueued})
		}
	}
	return jobs, nil
}

// classifyHistory maps a ComfyUI history status to a JobStatus. ComfyUI marks
// a finished prompt completed=true and records an "execution_error" or
// "execution_interrupted" message on failure/cancel.
func classifyHistory(completed bool, statusStr string, messages [][]any) JobStatus {
	for _, m := range messages {
		if len(m) == 0 {
			continue
		}
		kind, _ := m[0].(string)
		switch kind {
		case "execution_error":
			return StatusFailed
		case "execution_interrupted":
			return StatusCancelled
		}
	}
	switch statusStr {
	case "error":
		return StatusFailed
	case "interrupted", "cancelled":
		return StatusCancelled
	}
	if completed {
		return StatusCompleted
	}
	return StatusRunning
}

// errorsJoin joins errors into one message without requiring the errors
// package's Join semantics to be visible to callers.
func errorsJoin(errs ...error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	msg := ""
	for i, e := range errs {
		if i > 0 {
			msg += "; "
		}
		msg += e.Error()
	}
	return fmt.Errorf("comfy: %s", msg)
}
