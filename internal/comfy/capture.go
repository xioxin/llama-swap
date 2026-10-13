package comfy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Capture is a snapshot of one finished ComfyUI workflow, built from ComfyUI's
// GET /history/{prompt_id} record. It captures the small, durable facts — the
// workflow graphs, what ran, when, and which files were produced — so a capture
// stays viewable after ComfyUI restarts or its history is cleared. Output media
// is NOT embedded: it stays on ComfyUI's disk and is proxied live (see
// Output.URL).
type Capture struct {
	PromptID string `json:"prompt_id"`

	// WorkflowUI is the editor graph (extra_pnginfo.workflow): reopenable in
	// ComfyUI. WorkflowAPI is the executable node graph (prompt[2]). Both are
	// preserved verbatim so they can be viewed and downloaded.
	WorkflowUI  json.RawMessage `json:"workflow_ui,omitempty"`
	WorkflowAPI json.RawMessage `json:"workflow_api,omitempty"`

	Status  CaptureStatus `json:"status"`
	Nodes   []CaptureNode `json:"nodes"`
	Outputs []Output      `json:"outputs"`
}

// CaptureStatus is the execution outcome and timing of a workflow.
type CaptureStatus struct {
	StatusStr   string `json:"status_str"`
	Completed   bool   `json:"completed"`
	StartedAt   int64  `json:"started_at_ms"`  // 0 when unknown
	FinishedAt  int64  `json:"finished_at_ms"` // 0 when unknown
	DurationMs  int64  `json:"duration_ms"`
	CachedNodes int    `json:"cached_nodes"`
}

// CaptureNode summarises one node that ran, including any model files it named.
type CaptureNode struct {
	NodeID    string   `json:"node_id"`
	ClassType string   `json:"class_type"`
	Title     string   `json:"title,omitempty"`
	Models    []string `json:"models,omitempty"`
}

// Output is one produced file. URL and DownloadURL are llama-swap-relative
// proxy paths (built by the server); Kind drives preview-vs-download in the UI.
type Output struct {
	NodeID     string  `json:"node_id"`
	Filename   string  `json:"filename"`
	Subfolder  string  `json:"subfolder,omitempty"`
	Type       string  `json:"type,omitempty"` // ComfyUI dir type: output/temp/input
	Kind       string  `json:"kind,omitempty"` // image/video/audio/text/3d/...
	Mime       string  `json:"mime,omitempty"`
	Width      int     `json:"width,omitempty"`
	Height     int     `json:"height,omitempty"`
	Duration   float64 `json:"duration,omitempty"`
	FPS        float64 `json:"fps,omitempty"`
	FrameCount int     `json:"frame_count,omitempty"`

	URL         string `json:"url,omitempty"`          // proxy path to stream the file
	DownloadURL string `json:"download_url,omitempty"` // proxy path with attachment disposition
}

// historyRecord mirrors one entry of ComfyUI's GET /history/{id} value.
type historyRecord struct {
	Prompt  []json.RawMessage                  `json:"prompt"`
	Outputs map[string]map[string][]outputFile `json:"outputs"`
	Status  struct {
		StatusStr string              `json:"status_str"`
		Completed bool                `json:"completed"`
		Messages  [][]json.RawMessage `json:"messages"`
	} `json:"status"`
}

// outputFile mirrors one produced file entry in a history record.
type outputFile struct {
	Filename  string `json:"filename"`
	Subfolder string `json:"subfolder"`
	Type      string `json:"type"`
	Format    string `json:"format"`
	Fullpath  string `json:"fullpath"`
	Metadata  struct {
		Kind       string  `json:"kind"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		Duration   float64 `json:"duration"`
		FPS        float64 `json:"fps"`
		FrameCount int     `json:"frame_count"`
	} `json:"metadata"`
}

// ParseHistory builds a Capture from a GET /history/{prompt_id} JSON body of the
// form {"<prompt_id>": {...}}. It returns an error when the prompt id is absent
// or the record has no recognisable content.
func ParseHistory(body []byte) (*Capture, error) {
	var byID map[string]historyRecord
	if err := json.Unmarshal(body, &byID); err != nil {
		return nil, fmt.Errorf("comfy: parsing history: %w", err)
	}
	if len(byID) == 0 {
		return nil, fmt.Errorf("comfy: history record not found")
	}

	// Take the single record (ComfyUI keys by prompt id).
	var promptID string
	var rec historyRecord
	for id, r := range byID {
		promptID, rec = id, r
		break
	}

	cap := &Capture{PromptID: promptID}

	// prompt tuple: [number, prompt_id, api_graph, extra_data, outputs_to_run].
	if len(rec.Prompt) >= 3 {
		cap.WorkflowAPI = rec.Prompt[2]
	}
	if len(rec.Prompt) >= 4 {
		var extra struct {
			ExtraPNGInfo struct {
				Workflow json.RawMessage `json:"workflow"`
			} `json:"extra_pnginfo"`
		}
		if err := json.Unmarshal(rec.Prompt[3], &extra); err == nil {
			cap.WorkflowUI = extra.ExtraPNGInfo.Workflow
		}
	}

	cap.Status.StatusStr = rec.Status.StatusStr
	cap.Status.Completed = rec.Status.Completed
	cap.Status.CachedNodes, cap.Status.StartedAt, cap.Status.FinishedAt = parseMessages(rec.Status.Messages)
	if cap.Status.StartedAt > 0 && cap.Status.FinishedAt >= cap.Status.StartedAt {
		cap.Status.DurationMs = cap.Status.FinishedAt - cap.Status.StartedAt
	}

	cap.Nodes = parseNodes(rec.Prompt)
	cap.Outputs = parseOutputs(rec.Outputs)

	if cap.WorkflowUI == nil && cap.WorkflowAPI == nil && len(cap.Outputs) == 0 {
		return nil, fmt.Errorf("comfy: history record %q has no workflow or outputs", promptID)
	}
	return cap, nil
}

// parseMessages extracts cached-node count and the execution start/finish
// timestamps (ms) from the status message list.
func parseMessages(messages [][]json.RawMessage) (cached int, start, finish int64) {
	for _, m := range messages {
		if len(m) < 2 {
			continue
		}
		var kind string
		if err := json.Unmarshal(m[0], &kind); err != nil {
			continue
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(m[1], &payload); err != nil {
			continue
		}
		switch kind {
		case "execution_start":
			start = timestampOf(payload["timestamp"])
		case "execution_success", "execution_error", "execution_interrupted":
			finish = timestampOf(payload["timestamp"])
		case "execution_cached":
			var nodes []string
			if err := json.Unmarshal(payload["nodes"], &nodes); err == nil {
				cached = len(nodes)
			}
		}
	}
	return cached, start, finish
}

func timestampOf(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f)
	}
	return 0
}

// modelInputKeys are the input field names that hold a loaded model file name.
var modelInputKeys = map[string]bool{
	"unet_name": true, "vae_name": true, "clip_name": true,
	"ckpt_name": true, "model_name": true, "lora_name": true,
	"control_net_name": true, "upscale_model": true, "style_model_name": true,
}

// parseNodes summarises the executable graph (prompt[2]) into node + model info.
func parseNodes(prompt []json.RawMessage) []CaptureNode {
	if len(prompt) < 3 {
		return nil
	}
	var graph map[string]struct {
		ClassType string `json:"class_type"`
		Meta      struct {
			Title string `json:"title"`
		} `json:"_meta"`
		Inputs map[string]json.RawMessage `json:"inputs"`
	}
	if err := json.Unmarshal(prompt[2], &graph); err != nil {
		return nil
	}
	nodes := make([]CaptureNode, 0, len(graph))
	for id, n := range graph {
		node := CaptureNode{NodeID: id, ClassType: n.ClassType, Title: n.Meta.Title}
		for key, val := range n.Inputs {
			if !modelInputKeys[key] {
				continue
			}
			var name string
			if err := json.Unmarshal(val, &name); err == nil && name != "" {
				node.Models = append(node.Models, name)
			}
		}
		nodes = append(nodes, node)
	}
	sortNodes(nodes)
	return nodes
}

func sortNodes(nodes []CaptureNode) {
	for i := 1; i < len(nodes); i++ {
		for j := i; j > 0 && nodeLess(nodes[j], nodes[j-1]); j-- {
			nodes[j], nodes[j-1] = nodes[j-1], nodes[j]
		}
	}
}

func nodeLess(a, b CaptureNode) bool {
	ai, aerr := strconv.Atoi(a.NodeID)
	bi, berr := strconv.Atoi(b.NodeID)
	if aerr == nil && berr == nil {
		return ai < bi
	}
	return a.NodeID < b.NodeID
}

// parseOutputs flattens every produced file across nodes into Output entries.
func parseOutputs(outputs map[string]map[string][]outputFile) []Output {
	var list []Output
	for nodeID, byKey := range outputs {
		for _, files := range byKey {
			for _, f := range files {
				kind := f.Metadata.Kind
				if kind == "" {
					kind = kindFromFilename(f.Filename)
				}
				mime := f.Format
				if mime == "" {
					mime = mimeFromFilename(f.Filename)
				}
				list = append(list, Output{
					NodeID:     nodeID,
					Filename:   f.Filename,
					Subfolder:  f.Subfolder,
					Type:       f.Type,
					Kind:       kind,
					Mime:       mime,
					Width:      f.Metadata.Width,
					Height:     f.Metadata.Height,
					Duration:   f.Metadata.Duration,
					FPS:        f.Metadata.FPS,
					FrameCount: f.Metadata.FrameCount,
				})
			}
		}
	}
	sortOutputs(list)
	return list
}

func sortOutputs(list []Output) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].Filename < list[j-1].Filename; j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
}

// kindFromFilename infers a media kind when the metadata omitted it.
func kindFromFilename(name string) string {
	switch ext(name) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".avif":
		return "image"
	case ".mp4", ".webm", ".mov", ".mkv", ".avi":
		return "video"
	case ".mp3", ".wav", ".flac", ".ogg", ".m4a":
		return "audio"
	case ".json", ".txt", ".csv", ".md":
		return "text"
	case ".obj", ".fbx", ".glb", ".gltf", ".ply", ".stl", ".usdz":
		return "3d"
	}
	return "file"
}

func mimeFromFilename(name string) string {
	switch ext(name) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	case ".mp4":
		return "video/mp4"
	case ".webm":
		return "video/webm"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}

func ext(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i:])
}

// SetProxyURLs fills each Output's URL and DownloadURL with llama-swap proxy
// paths that stream the file from ComfyUI. proxyPath is the base path of the
// proxy endpoint (e.g. "/api/comfy/view").
func (c *Capture) SetProxyURLs(proxyPath string) {
	for i := range c.Outputs {
		c.Outputs[i].URL = outputProxyURL(proxyPath, c.Outputs[i], false)
		c.Outputs[i].DownloadURL = outputProxyURL(proxyPath, c.Outputs[i], true)
	}
}

func outputProxyURL(base string, o Output, download bool) string {
	q := url.Values{}
	q.Set("filename", o.Filename)
	if o.Subfolder != "" {
		q.Set("subfolder", o.Subfolder)
	}
	if o.Type != "" {
		q.Set("type", o.Type)
	}
	if download {
		q.Set("download", "1")
	}
	if strings.Contains(base, "?") {
		return base + "&" + q.Encode()
	}
	return base + "?" + q.Encode()
}

// History fetches and parses the capture snapshot for a prompt id from
// ComfyUI's GET /history/{prompt_id}. It returns nil (with the error) when the
// record is missing or ComfyUI is unreachable.
func (c *Client) History(ctx context.Context, promptID string) (*Capture, error) {
	if promptID == "" {
		return nil, fmt.Errorf("comfy: empty prompt id")
	}
	var raw json.RawMessage
	if err := c.getJSON(ctx, "/history/"+promptID, &raw); err != nil {
		return nil, err
	}
	return ParseHistory(raw)
}

// View issues GET /api/view with the given query and returns the raw upstream
// response so the caller can stream it (media must not be buffered). The caller
// closes the response body. filename/subfolder/type are passed through as query
// parameters; ComfyUI validates and resolves them on its side.
func (c *Client) View(ctx context.Context, q url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/view?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("comfy: building GET /api/view: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("comfy: GET /api/view: %w", err)
	}
	return resp, nil
}
