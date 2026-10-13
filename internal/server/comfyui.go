package server

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ComfyUI compatibility endpoint. ComfyUI is a web app rather than an
// OpenAI-compatible server, so it is served through its own passthrough bound
// to the reserved config.ComfyUIModelID model. Routes are registered in
// server.go; the model's compatibility settings are applied while loading
// config.

func handleComfyUIRedirect(w http.ResponseWriter, r *http.Request) {
	location := "/comfyui/"
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	status := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusMovedPermanently
	}
	http.Redirect(w, r, location, status)
}

// comfyUIIgnorePaths are the paths under /comfyui/ that may not start an
// unloaded model. An open ComfyUI tab retries them on its own, so letting them
// swap the model in would undo every unload while a browser is pointed at it.
//
// This is the built-in equivalent of the upstream.ignorePaths a second ComfyUI
// instance needs when it is served through /upstream/<model>/ instead, so the
// two lists are kept the same: the shared static-asset default, the frontend
// websocket, and its job polling.
var comfyUIIgnorePaths = append(
	config.DefaultUpstreamIgnorePaths(),
	regexp.MustCompile(`^/ws(/|$)`),
	regexp.MustCompile(`^/api/jobs$`),
)

// comfyUIIgnoresPath reports whether path is one that may not start the model.
// path is the decoded path only, so query parameters such as ?clientId= never
// affect the match.
func comfyUIIgnoresPath(path string) bool {
	for _, re := range comfyUIIgnorePaths {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// handleComfyUI proxies requests under /comfyui/ to the fixed local
// ComfyUI model. Its compatibility settings are applied while loading config.
// It only strips the /comfyui prefix and then defers to serveComfyUI.
func (s *Server) handleComfyUI(w http.ResponseWriter, r *http.Request) {
	// Strip the /comfyui prefix before forwarding. URL.Path and PathValue are
	// decoded, so retain the matching escaped suffix in RawPath exactly as the
	// generic /upstream handler does.
	remainingPath := "/" + strings.TrimPrefix(r.PathValue("comfyPath"), "/")
	escapedRemaining := swaputil.EscapedPathSuffix(r.URL.EscapedPath(), "/comfyui")
	r.URL.Path = remainingPath
	r.URL.RawPath = escapedRemaining
	s.serveComfyUI(w, r, false)
}

// serveComfyUI proxies a request to the fixed local ComfyUI model and serves
// it at the path root of the port it is received on. r.URL.Path must already
// be ComfyUI-root-relative: handleComfyUI strips the /comfyui prefix, and
// requests on the dedicated comfyui.port listener arrive root-relative.
//
// Both surfaces share the same start gating: while the model is not loaded, a
// GET to an ignored path cannot start it (see comfyUIIgnoresPath).
func (s *Server) serveComfyUI(w http.ResponseWriter, r *http.Request, dedicated bool) {
	if _, ok := s.cfg.Models[config.ComfyUIModelID]; !ok || !s.local.Handles(config.ComfyUIModelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "local model "+config.ComfyUIModelID+" not found")
		return
	}

	*r = *r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ApiKey:   swaputil.ExtractAPIKey(r),
		Model:    config.ComfyUIModelID,
		ModelID:  config.ComfyUIModelID,
		Metadata: make(map[string]string),
	}))

	// Direct (non-submit) requests are served to ComfyUI without ever evicting
	// another model: they may START ComfyUI into available headroom and hold the
	// request until it is ready, but they must never swap a running LLM out. That
	// is what stops a history panel poll or a settings write from unloading a
	// model. Only POST /prompt (below) may swap.
	if isComfyDirect(r) {
		// Start gating: on the /comfyui/ surface an ignored-path GET may not
		// start the model (kept from the passthrough rules). The dedicated port
		// cold-starts freely on any request.
		if !dedicated && r.Method == http.MethodGet && comfyUIIgnoresPath(r.URL.Path) {
			if p, ok := s.local.DirectProcess(config.ComfyUIModelID); ok {
				p.ServeHTTP(w, r)
				return
			}
			swaputil.SendResponse(w, r, http.StatusConflict,
				"model "+config.ComfyUIModelID+" is not loaded; ignored ComfyUI paths cannot start it")
			return
		}
		// Serve directly when already running. When not running, start it: by
		// default into free headroom without evicting (EnsureDirect); with
		// comfyui.startEvicts, through the scheduler which may reclaim a
		// sibling model's GPU if headroom is insufficient.
		if p, ok := s.local.DirectProcess(config.ComfyUIModelID); ok {
			p.ServeHTTP(w, r)
			return
		}
		if s.cfg.ComfyUI != nil && s.cfg.ComfyUI.StartEvicts {
			s.local.ServeHTTP(w, r) // scheduler may swap to make room
			return
		}
		if p, ok := s.local.EnsureDirect(config.ComfyUIModelID, r.Context()); ok {
			p.ServeHTTP(w, r)
			return
		}
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable,
			"model "+config.ComfyUIModelID+" is not loaded and did not start")
		return
	}

	// POST /prompt: the one request that queues for the GPU and swaps. A GET to
	// an ignored path never starts the model (kept from the passthrough rules).
	if r.Method == http.MethodGet && comfyUIIgnoresPath(r.URL.Path) {
		state, ok := s.local.RunningModels()[config.ComfyUIModelID]
		if !ok || state != process.StateReady {
			swaputil.SendResponse(w, r, http.StatusConflict,
				"model "+config.ComfyUIModelID+" is not loaded; ignored ComfyUI paths cannot start it")
			return
		}
	}

	// Bound how long a submit may sit in the swap queue. On timeout the request
	// context is cancelled, which prunes the queued entry and lets the client
	// retry instead of hanging forever behind a stuck swap.
	if wait := s.cfg.ComfyUI.QueueWait(); wait > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), wait)
		defer cancel()
		r = r.WithContext(ctx)
	}
	recorder := wrapComfySubmit(w)
	s.local.ServeHTTP(recorder, r)
	if !recorder.WroteHeader() {
		// The queue wait timed out (or the client left) before the request
		// reached ComfyUI: report 503 so the submit is visibly rejected rather
		// than leaving the connection to hang.
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable,
			"comfyui: workflow submit timed out waiting for the queue")
		return
	}
	recordComfySubmit(s, r, recorder)
}
