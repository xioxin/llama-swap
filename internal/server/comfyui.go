package server

import (
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
	s.serveComfyUI(w, r)
}

// serveComfyUI proxies a request to the fixed local ComfyUI model and serves
// it at the path root of the port it is received on. r.URL.Path must already
// be ComfyUI-root-relative: handleComfyUI strips the /comfyui prefix, and
// requests on the dedicated comfyui.port listener arrive root-relative.
//
// Both surfaces share the same start gating: while the model is not loaded, a
// GET to an ignored path cannot start it (see comfyUIIgnoresPath).
func (s *Server) serveComfyUI(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.cfg.Models[config.ComfyUIModelID]; !ok || !s.local.Handles(config.ComfyUIModelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "local model "+config.ComfyUIModelID+" not found")
		return
	}

	// Only a GET is ignored. A write is a deliberate action even on an ignored
	// path, so queueing a job still starts the model.
	if r.Method == http.MethodGet && comfyUIIgnoresPath(r.URL.Path) {
		state, ok := s.local.RunningModels()[config.ComfyUIModelID]
		if !ok || state != process.StateReady {
			swaputil.SendResponse(w, r, http.StatusConflict,
				"model "+config.ComfyUIModelID+" is not loaded; ignored ComfyUI paths cannot start it")
			return
		}
	}

	*r = *r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ApiKey:   swaputil.ExtractAPIKey(r),
		Model:    config.ComfyUIModelID,
		ModelID:  config.ComfyUIModelID,
		Metadata: make(map[string]string),
	}))

	// A workflow submit is a task: it goes through the router's queue (so it
	// waits for a mutually exclusive model to give up the GPU) and is tracked
	// to its terminal state for the activity log. Tee the response so the
	// prompt id can be read after the upstream writes it. Every other ComfyUI
	// path is proxied straight through.
	if isComfyPromptSubmit(r) {
		recorder := wrapComfySubmit(w)
		s.local.ServeHTTP(recorder, r)
		recordComfySubmit(s, r, recorder)
		return
	}
	s.local.ServeHTTP(w, r)
}
