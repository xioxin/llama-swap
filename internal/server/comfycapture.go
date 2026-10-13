package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleAPIComfyView streams one ComfyUI output file from the running ComfyUI
// process. The query (filename/subfolder/type) is passed through to ComfyUI's
// /api/view, which resolves and validates it on its side. Media is streamed,
// never buffered. ?download=1 adds an attachment disposition so the browser
// saves rather than renders the file.
func (s *Server) handleAPIComfyView(w http.ResponseWriter, r *http.Request) {
	client := s.comfyClient()
	if client == nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, "comfyui is not configured")
		return
	}

	q := url.Values{}
	for _, key := range []string{"filename", "subfolder", "type"} {
		if v := r.URL.Query().Get(key); v != "" {
			q.Set(key, v)
		}
	}
	if q.Get("filename") == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "filename is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()

	// Wake ComfyUI if it is not running and hold this request until it is
	// ready, so a preview/download never fails with "unreachable" just because
	// ComfyUI was cold. EnsureDirect starts into free headroom without evicting
	// another model (see comfyui.startEvicts), matching a direct passthrough.
	if s.local != nil {
		if _, ok := s.local.DirectProcess(config.ComfyUIModelID); !ok {
			if _, ok := s.local.EnsureDirect(config.ComfyUIModelID, ctx); !ok {
				swaputil.SendResponse(w, r, http.StatusServiceUnavailable,
					"comfyui is not loaded and did not start")
				return
			}
		}
	}

	resp, err := client.View(ctx, q)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadGateway, "comfyui is unreachable")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		swaputil.SendResponse(w, r, resp.StatusCode, "comfyui returned an error for this file")
		return
	}

	// Forward the content headers the browser needs for rendering or saving.
	for _, key := range []string{"Content-Type", "Content-Length", "Accept-Ranges", "Content-Range", "Etag", "Last-Modified"} {
		if v := resp.Header.Get(key); v != "" {
			w.Header().Set(key, v)
		}
	}
	if r.URL.Query().Get("download") == "1" {
		name := q.Get("filename")
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	} else {
		// Inline: let the browser render images/videos; still force a filename
		// for unknown types so they download predictably.
		name := q.Get("filename")
		w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleAPICapture returns the stored capture for a metric (activity row) ID.
// ComfyUI rows return a workflow/output snapshot (kind "comfy"); other rows
// return the request/response capture (kind "reqresp"). The kind field lets the
// UI pick the right renderer.
func (s *Server) handleAPICapture(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid capture ID")
		return
	}

	if cap := s.metrics.getComfyCaptureByID(id); cap != nil {
		writeCapture(w, CaptureKindComfy, cap)
		return
	}
	capture := s.metrics.getCaptureByID(id)
	if capture == nil {
		swaputil.SendResponse(w, r, http.StatusNotFound, "capture not found")
		return
	}
	writeCapture(w, CaptureKindReqResp, capture)
}

// captureEnvelope tags a capture payload with its kind so the client can
// deserialize the right shape.
type captureEnvelope struct {
	Kind    string `json:"kind"`
	Payload any    `json:"payload"`
}

func writeCapture(w http.ResponseWriter, kind CaptureKind, payload any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(captureEnvelope{Kind: string(kind), Payload: payload})
}
