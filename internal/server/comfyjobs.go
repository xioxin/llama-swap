package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/mostlygeek/llama-swap/internal/comfy"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// comfyJobView is one ComfyUI workflow task as reported by GET /api/comfy/jobs.
// It merges ComfyUI's live queue (queued/running) with recently tracked
// terminal rows so the panel shows waiting, executing and finished work.
type comfyJobView struct {
	PromptID   string `json:"prompt_id"`
	Status     string `json:"status"` // queued|running|completed|failed|cancelled|unknown
	Model      string `json:"model"`
	ReqPath    string `json:"req_path"`
	Timestamp  string `json:"timestamp"`
	DurationMs int    `json:"duration_ms"`
	ErrorMsg   string `json:"error_msg,omitempty"`
	Live       bool   `json:"live"` // true while ComfyUI still has the prompt
}

// handleAPIComfyJobs lists ComfyUI workflow tasks. Live queued/running prompts
// come from ComfyUI's own queue; recent terminal states come from the activity
// rows the task tracker backfilled. It is a read-only view: reaching it never
// starts or swaps a model.
func (s *Server) handleAPIComfyJobs(w http.ResponseWriter, r *http.Request) {
	jobs := s.comfyJobsView(r.Context())

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"jobs": jobs})
}

// comfyJobsView builds the merged task list. Live jobs are authoritative for
// queued/running; activity rows fill in terminal history and any tracked prompt
// that has since gone quiet.
func (s *Server) comfyJobsView(ctx context.Context) []comfyJobView {
	byID := map[string]comfyJobView{}
	var order []string

	add := func(v comfyJobView) {
		if v.PromptID == "" {
			return
		}
		if _, exists := byID[v.PromptID]; !exists {
			order = append(order, v.PromptID)
		}
		byID[v.PromptID] = v
	}

	// Live work from ComfyUI's queue (best effort: a down ComfyUI simply
	// contributes no live rows).
	if client := s.comfyClient(); client != nil {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if live, err := client.Jobs(cctx); err == nil {
			for _, j := range live {
				add(comfyJobView{
					PromptID: j.PromptID,
					Status:   string(j.Status),
					Model:    config.ComfyUIModelID,
					ReqPath:  comfyPromptPath,
					Live:     true,
				})
			}
		}
	}

	// Terminal/known states from tracked activity rows.
	page, err := s.store.Activity().List(ctx, store.ActivityQuery{
		ActivityFilter: store.ActivityFilter{Models: []string{config.ComfyUIModelID}},
		Limit:          50,
	})
	if err == nil {
		for _, e := range page.Data {
			promptID := e.Metadata[metaComfyPromptID]
			if promptID == "" {
				continue
			}
			status := e.Metadata[metaComfyStatus]
			if status == "" {
				status = string(comfy.StatusUnknown)
			}
			view := comfyJobView{
				PromptID:   promptID,
				Status:     status,
				Model:      e.Model,
				ReqPath:    e.ReqPath,
				Timestamp:  e.Timestamp.Format(time.RFC3339),
				DurationMs: e.DurationMs,
				ErrorMsg:   e.ErrorMsg,
			}
			// A live row already present wins for status (still queued/running).
			if existing, ok := byID[promptID]; ok && existing.Live {
				continue
			}
			add(view)
		}
	}

	out := make([]comfyJobView, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	return out
}
