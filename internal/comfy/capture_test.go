package comfy

import (
	"encoding/json"
	"strings"
	"testing"
)

// historyFixture is a trimmed but structurally faithful GET /history/{id}
// response, matching the real ComfyUI schema: prompt tuple (number, id, api
// graph, extra_data with extra_pnginfo.workflow), outputs keyed by node then
// media key, and status with execution messages.
const historyFixture = `{
  "5bf2ef53-aa37-4c8f-80e5-36cea152b698": {
    "prompt": [
      0,
      "5bf2ef53-aa37-4c8f-80e5-36cea152b698",
      {
        "44": {"inputs": {"unet_name": "Anima/novaAnimeAM_v15.safetensors", "weight_dtype": "default"}, "class_type": "UNETLoader", "_meta": {"title": "Load Diffusion Model"}},
        "15": {"inputs": {"vae_name": "Qwen/qwen_image_vae.safetensors"}, "class_type": "VAELoader", "_meta": {"title": "Load VAE"}},
        "19": {"inputs": {"seed": 1, "steps": 30, "model": ["44", 0]}, "class_type": "KSampler", "_meta": {"title": "KSampler"}},
        "47": {"inputs": {"images": ["8", 0]}, "class_type": "SaveImage", "_meta": {"title": "Save"}}
      },
      {
        "comfy_usage_source": "comfyui-frontend",
        "extra_pnginfo": {
          "workflow": {
            "id": "abc",
            "last_node_id": 48,
            "nodes": [{"id": 47, "type": "SaveImage"}],
            "links": [],
            "version": 0.4
          }
        },
        "client_id": "cid",
        "create_time": 1791899307000
      },
      []
    ],
    "outputs": {
      "47": {
        "images": [
          {"filename": "ComfyUI_00040_.png", "subfolder": "", "type": "output", "metadata": {"kind": "image", "width": 1664, "height": 1248}, "id": "115fb2ef"}
        ]
      }
    },
    "status": {
      "status_str": "success",
      "completed": true,
      "messages": [
        ["execution_start", {"prompt_id": "5bf2ef53-aa37-4c8f-80e5-36cea152b698", "timestamp": 1791899307174}],
        ["execution_cached", {"nodes": ["11", "12", "15"], "prompt_id": "5bf2ef53-aa37-4c8f-80e5-36cea152b698", "timestamp": 1791899307178}],
        ["execution_success", {"prompt_id": "5bf2ef53-aa37-4c8f-80e5-36cea152b698", "timestamp": 1791899324764}]
      ]
    },
    "meta": {"47": {"node_id": "47", "display_node": "47", "real_node_id": "47"}}
  }
}`

func TestComfy_ParseHistory_ParsesFullRecord(t *testing.T) {
	cap, err := ParseHistory([]byte(historyFixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if cap.PromptID != "5bf2ef53-aa37-4c8f-80e5-36cea152b698" {
		t.Errorf("PromptID = %q", cap.PromptID)
	}
	if len(cap.WorkflowUI) == 0 || len(cap.WorkflowAPI) == 0 {
		t.Errorf("workflows not captured: ui=%d api=%d bytes", len(cap.WorkflowUI), len(cap.WorkflowAPI))
	}

	// Status + timing: 1791899324764 - 1791899307174 = 17590ms, 3 cached nodes.
	if cap.Status.StatusStr != "success" || !cap.Status.Completed {
		t.Errorf("status = %+v, want success/completed", cap.Status)
	}
	if cap.Status.StartedAt != 1791899307174 || cap.Status.FinishedAt != 1791899324764 {
		t.Errorf("timing = %d..%d", cap.Status.StartedAt, cap.Status.FinishedAt)
	}
	if cap.Status.DurationMs != 17590 {
		t.Errorf("DurationMs = %d, want 17590", cap.Status.DurationMs)
	}
	if cap.Status.CachedNodes != 3 {
		t.Errorf("CachedNodes = %d, want 3", cap.Status.CachedNodes)
	}

	// Nodes + models: unet_name and vae_name picked up.
	models := map[string]bool{}
	for _, n := range cap.Nodes {
		for _, m := range n.Models {
			models[m] = true
		}
	}
	if !models["Anima/novaAnimeAM_v15.safetensors"] || !models["Qwen/qwen_image_vae.safetensors"] {
		t.Errorf("models = %v, want unet+vae names", models)
	}

	// One output, image kind with dimensions.
	if len(cap.Outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(cap.Outputs))
	}
	o := cap.Outputs[0]
	if o.Filename != "ComfyUI_00040_.png" || o.Kind != "image" || o.Width != 1664 || o.Height != 1248 {
		t.Errorf("output = %+v", o)
	}
}

func TestComfy_ParseHistory_MultiOutputVideos(t *testing.T) {
	fixture := `{
	  "p2": {
	    "prompt": [0, "p2", {"4": {"inputs": {}, "class_type": "VHS_VideoCombine", "_meta": {"title": "Video"}}}, {"extra_pnginfo": {"workflow": {"nodes": []}}}, []],
	    "outputs": {
	      "5": {"gifs": [{"filename": "AnimateDiff_00003.mp4", "subfolder": "", "type": "output", "format": "video/h264-mp4", "metadata": {"kind": "video", "width": 480, "height": 856, "duration": 3.75, "fps": 8.0, "frame_count": 30}, "id": "a"}]},
	      "4": {"gifs": [{"filename": "AnimateDiff_00002-audio.mp4", "subfolder": "sub", "type": "output", "format": "video/h264-mp4", "metadata": {"kind": "video", "width": 480, "height": 856, "duration": 1.0, "fps": 30.0, "frame_count": 30}, "id": "b"}]}
	    },
	    "status": {"status_str": "success", "completed": true, "messages": [["execution_start", {"timestamp": 100}], ["execution_success", {"timestamp": 400}]]}
	  }
	}`
	cap, err := ParseHistory([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(cap.Outputs) != 2 {
		t.Fatalf("outputs = %d, want 2 (multi-node)", len(cap.Outputs))
	}
	byName := map[string]Output{}
	for _, o := range cap.Outputs {
		byName[o.Filename] = o
	}
	if o := byName["AnimateDiff_00003.mp4"]; o.Kind != "video" || o.FrameCount != 30 || o.FPS != 8 {
		t.Errorf("video1 = %+v", o)
	}
	if o := byName["AnimateDiff_00002-audio.mp4"]; o.Subfolder != "sub" || o.NodeID != "4" {
		t.Errorf("video2 = %+v", o)
	}
	if cap.Status.DurationMs != 300 {
		t.Errorf("DurationMs = %d, want 300", cap.Status.DurationMs)
	}
}

func TestComfy_SetProxyURLs(t *testing.T) {
	cap := &Capture{Outputs: []Output{
		{Filename: "a.png", Type: "output"},
		{Filename: "b.mp4", Subfolder: "s", Type: "output"},
		{NodeID: "3", Kind: "text", Text: "hi"},
	}}
	cap.SetProxyURLs("/api/comfy/view")

	if got := cap.Outputs[0].URL; got != "/api/comfy/view?filename=a.png&type=output" {
		t.Errorf("url = %q", got)
	}
	if got := cap.Outputs[0].DownloadURL; got != "/api/comfy/view?download=1&filename=a.png&type=output" {
		t.Errorf("download = %q", got)
	}
	if got := cap.Outputs[1].URL; got != "/api/comfy/view?filename=b.mp4&subfolder=s&type=output" {
		t.Errorf("subfolder url = %q", got)
	}
	// Text results live in the capture and are never streamed from ComfyUI.
	if cap.Outputs[2].URL != "" || cap.Outputs[2].DownloadURL != "" {
		t.Errorf("text output got proxy URLs: %+v", cap.Outputs[2])
	}
}

func TestComfy_ParseHistory_Empty(t *testing.T) {
	if _, err := ParseHistory([]byte(`{}`)); err == nil {
		t.Errorf("ParseHistory({}) = nil error, want error")
	}
}

func TestComfy_CaptureJSONRoundTrip(t *testing.T) {
	cap, err := ParseHistory([]byte(historyFixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	b, err := json.Marshal(cap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Capture
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.PromptID != cap.PromptID || len(out.Outputs) != 1 {
		t.Errorf("round trip mismatch: %+v", out)
	}
}

// TestComfy_ParseHistory_NonNilArrays proves outputs and nodes are always
// non-nil arrays so the UI never reads .length on null for a bare/cancelled
// record.
func TestComfy_ParseHistory_NonNilArrays(t *testing.T) {
	// A record with a workflow but no outputs and an empty graph.
	fixture := `{
	  "x": {
	    "prompt": [0, "x", {}, {"extra_pnginfo": {"workflow": {"nodes": []}}}, []],
	    "outputs": {},
	    "status": {"status_str": "success", "completed": true, "messages": []}
	  }
	}`
	cap, err := ParseHistory([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if cap.Outputs == nil {
		t.Errorf("Outputs = nil, want empty slice")
	}
	if cap.Nodes == nil {
		t.Errorf("Nodes = nil, want empty slice")
	}
	if len(cap.Outputs) != 0 || len(cap.Nodes) != 0 {
		t.Errorf("outputs=%d nodes=%d, want empty", len(cap.Outputs), len(cap.Nodes))
	}
	// And they survive a JSON round trip as [] not null.
	b, _ := json.Marshal(cap)
	if strings.Contains(string(b), `"outputs":null`) || strings.Contains(string(b), `"nodes":null`) {
		t.Errorf("serialized null arrays: %s", b)
	}
}

// historyFixturePrimitiveOutputs mirrors a real ComfyUI history record whose
// graph includes primitive nodes. Their results ("value": [8]) live in the same
// outputs map as media files; the strict file-only decode used to reject the
// whole record, so a finished workflow ended up with no capture.
const historyFixturePrimitiveOutputs = `{
  "1f0e50b5-69ff-497b-86a3-3c3dc7202cca": {
    "prompt": [
      0,
      "1f0e50b5-69ff-497b-86a3-3c3dc7202cca",
      {
        "5693": {"inputs": {"expression": "a+b"}, "class_type": "MathExpression|pysssss", "_meta": {"title": "Math Expression"}},
        "5814": {"inputs": {"filename_prefix": "beach/x", "images": ["5741_5740", 0]}, "class_type": "VideoCombineV2", "_meta": {"title": "Video Combine"}}
      },
      {"comfy_usage_source": "comfy-cli/comfy-mcp", "client_id": "cid", "create_time": 1795179856733},
      ["5693", "5814"]
    ],
    "outputs": {
      "5693": {"value": [8]},
      "5741_5725": {"value": [1472]},
      "5741_5726": {"value": [1088]},
      "5814": {"gifs": [{
        "filename": "01 girl - Part 1 _v5_00001-audio.mp4",
        "subfolder": "beach",
        "type": "output",
        "format": "video/h264-mp4",
        "fullpath": "/home/user/comfy/ComfyUI/output/beach/01 girl - Part 1 _v5_00001-audio.mp4",
        "metadata": {"kind": "video", "width": 1472, "height": 1088, "duration": 8.0, "fps": 24.0, "frame_count": 192}
      }]}
    },
    "status": {
      "status_str": "success",
      "completed": true,
      "messages": [
        ["execution_start", {"prompt_id": "1f0e50b5-69ff-497b-86a3-3c3dc7202cca", "timestamp": 1795179856734}],
        ["execution_cached", {"nodes": [], "prompt_id": "1f0e50b5-69ff-497b-86a3-3c3dc7202cca", "timestamp": 1795179856758}],
        ["execution_success", {"prompt_id": "1f0e50b5-69ff-497b-86a3-3c3dc7202cca", "timestamp": 1795180133221}]
      ]
    },
    "meta": {"5693": {"node_id": "5693"}}
  }
}`

// TestComfy_ParseHistory_PrimitiveOutputs is the regression guard for captures
// that went missing because a primitive node's "value" output made the whole
// history record fail to decode.
func TestComfy_ParseHistory_PrimitiveOutputs(t *testing.T) {
	cap, err := ParseHistory([]byte(historyFixturePrimitiveOutputs))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if cap.PromptID != "1f0e50b5-69ff-497b-86a3-3c3dc7202cca" {
		t.Errorf("PromptID = %q", cap.PromptID)
	}
	if len(cap.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2", len(cap.Nodes))
	}
	if len(cap.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want 1 file (primitive values are not files)", cap.Outputs)
	}
	o := cap.Outputs[0]
	if o.Filename != "01 girl - Part 1 _v5_00001-audio.mp4" || o.Kind != "video" || o.FrameCount != 192 || o.Width != 1472 {
		t.Errorf("output = %+v", o)
	}
	if cap.Status.DurationMs != 276487 {
		t.Errorf("DurationMs = %d, want 276487", cap.Status.DurationMs)
	}
	if len(cap.WorkflowAPI) == 0 {
		t.Error("WorkflowAPI empty, want the submitted graph")
	}
	if len(cap.WorkflowUI) != 0 {
		t.Errorf("WorkflowUI = %d bytes, want none for an API-client submission", len(cap.WorkflowUI))
	}
}

// TestComfy_ParseHistory_NoFileOutputs proves a completed workflow that produced
// no files still gets a capture: the workflow graph is enough to make it useful.
func TestComfy_ParseHistory_NoFileOutputs(t *testing.T) {
	fixture := `{
	  "x": {
	    "prompt": [0, "x", {"1": {"inputs": {}, "class_type": "KSampler", "_meta": {"title": "KSampler"}}}, {"extra_pnginfo": {"workflow": {"nodes": []}}}, []],
	    "outputs": {},
	    "status": {"status_str": "success", "completed": true, "messages": [
	      ["execution_start", {"timestamp": 100}],
	      ["execution_success", {"timestamp": 350}]
	    ]}
	  }
	}`
	cap, err := ParseHistory([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if cap.Outputs == nil || len(cap.Outputs) != 0 {
		t.Errorf("outputs = %#v, want a non-nil empty slice", cap.Outputs)
	}
	if len(cap.WorkflowAPI) == 0 || len(cap.WorkflowUI) == 0 {
		t.Errorf("workflows not captured: api=%d ui=%d", len(cap.WorkflowAPI), len(cap.WorkflowUI))
	}
	if cap.Status.DurationMs != 250 {
		t.Errorf("DurationMs = %d, want 250", cap.Status.DurationMs)
	}
}

// TestComfy_ParseHistory_KeepsTextSkipsJunk proves a text result is kept as an
// output while text/primitive/malformed items that cannot be rendered are
// ignored individually instead of failing the record.
func TestComfy_ParseHistory_KeepsTextSkipsJunk(t *testing.T) {
	fixture := `{
	  "x": {
	    "prompt": [0, "x", {}, {}, []],
	    "outputs": {
	      "1": {"text": ["hello"]},
	      "2": {"value": [1, 2]},
	      "3": {"files": ["not-a-file", null, {"filename": "a.png", "subfolder": "", "type": "output"}]},
	      "4": {"images": [{"filename": "b.png", "type": "output", "metadata": {"kind": "image"}}]}
	    },
	    "status": {"status_str": "success", "completed": true, "messages": []}
	  }
	}`
	cap, err := ParseHistory([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(cap.Outputs) != 3 {
		t.Fatalf("outputs = %+v, want a.png, b.png and the hello text", cap.Outputs)
	}
	// Files keep their historical filename order and come before text results.
	names := []string{cap.Outputs[0].Filename, cap.Outputs[1].Filename}
	if names[0] != "a.png" || names[1] != "b.png" {
		t.Errorf("filenames = %v", names)
	}
	text := cap.Outputs[2]
	if text.NodeID != "1" || text.Kind != "text" || text.Text != "hello" {
		t.Errorf("text output = %+v", text)
	}
}

// historyFixtureTextOutput is a trimmed but structurally faithful record from a
// PreviewAny node: its result lives in the per-node outputs map under "text",
// alongside (not nested in) the file entries SaveImage-style nodes produce.
const historyFixtureTextOutput = `{
  "6ef4575c-b6a8-430b-a219-7adab908e08d": {
    "prompt": [
      2,
      "6ef4575c-b6a8-430b-a219-7adab908e08d",
      {
        "5": {"inputs": {"image": ["7", 0]}, "class_type": "Booru Tagger", "_meta": {"title": "Booru Tagger"}},
        "6": {"inputs": {"source": ["5", 0]}, "class_type": "PreviewAny", "_meta": {"title": "PreviewAny"}},
        "7": {"inputs": {"image": "Qwen_image_2.1_00006.png"}, "class_type": "LoadImage", "_meta": {"title": "Load Image"}}
      },
      {"comfy_usage_source": "comfyui-frontend", "extra_pnginfo": {"workflow": {"nodes": []}}},
      ["6"]
    ],
    "outputs": {
      "6": {"text": ["street fighter, shin sangoku musou, original, chun-li, hair bun, chinese clothes, 2girls, multiple girls, double bun, black hair, fighting stance, dress, fighting, china dress, pantyhose, kicking, pants, martial arts, smile, battle, fire, shoes, white shirt, purple hair, brown eyes, green dress, red shoes, red eyes, magic, red pants, black pantyhose, short hair, shirt, red ribbon, long sleeves, breasts, ribbon, tangzhuang, flying kick, "]}
    },
    "status": {
      "status_str": "success",
      "completed": true,
      "messages": [
        ["execution_start", {"prompt_id": "6ef4575c-b6a8-430b-a219-7adab908e08d", "timestamp": 1795680808712}],
        ["execution_success", {"prompt_id": "6ef4575c-b6a8-430b-a219-7adab908e08d", "timestamp": 1795680811791}]
      ]
    }
  }
}`

// TestComfy_ParseHistory_TextOutput keeps a PreviewAny-style text result in the
// capture (it used to be dropped, leaving the Outputs tab empty) and gives it no
// proxy URL: the text is stored in the capture, not streamed from ComfyUI.
func TestComfy_ParseHistory_TextOutput(t *testing.T) {
	const wantText = "street fighter, shin sangoku musou, original, chun-li, hair bun, chinese clothes, 2girls, multiple girls, double bun, black hair, fighting stance, dress, fighting, china dress, pantyhose, kicking, pants, martial arts, smile, battle, fire, shoes, white shirt, purple hair, brown eyes, green dress, red shoes, red eyes, magic, red pants, black pantyhose, short hair, shirt, red ribbon, long sleeves, breasts, ribbon, tangzhuang, flying kick, "

	cap, err := ParseHistory([]byte(historyFixtureTextOutput))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	if len(cap.Outputs) != 1 {
		t.Fatalf("outputs = %+v, want 1 text output", cap.Outputs)
	}
	o := cap.Outputs[0]
	if o.NodeID != "6" || o.Kind != "text" || o.Text != wantText {
		t.Errorf("output = %+v, want node 6 text", o)
	}
	if o.Filename != "" || o.URL != "" || o.DownloadURL != "" {
		t.Errorf("text output has file fields: %+v", o)
	}
	cap.SetProxyURLs("/api/comfy/view")
	if cap.Outputs[0].URL != "" || cap.Outputs[0].DownloadURL != "" {
		t.Errorf("text output got proxy URLs: %+v", cap.Outputs[0])
	}
	if len(cap.WorkflowAPI) == 0 || len(cap.WorkflowUI) == 0 {
		t.Errorf("workflows not captured: api=%d ui=%d", len(cap.WorkflowAPI), len(cap.WorkflowUI))
	}
	if len(cap.Nodes) != 3 {
		t.Errorf("nodes = %+v, want the three graph nodes", cap.Nodes)
	}
	if cap.Status.DurationMs != 3079 {
		t.Errorf("DurationMs = %d, want 3079", cap.Status.DurationMs)
	}
}

// TestComfy_ParseHistory_TextVariants covers the item shapes a text output can
// take: several items under one key, the "string" key alias, non-string scalars
// and values that must not become blank or structured cards.
func TestComfy_ParseHistory_TextVariants(t *testing.T) {
	fixture := `{
	  "x": {
	    "prompt": [0, "x", {}, {}, []],
	    "outputs": {
	      "1": {"text": ["first", "", "   "]},
	      "2": {"string": ["second"]},
	      "3": {"text": [42, true, null, {"a": 1}, ["nested"]]}
	    },
	    "status": {"status_str": "success", "completed": true, "messages": []}
	  }
	}`
	cap, err := ParseHistory([]byte(fixture))
	if err != nil {
		t.Fatalf("ParseHistory: %v", err)
	}
	want := []Output{
		{NodeID: "1", Kind: "text", Text: "first"},
		{NodeID: "2", Kind: "text", Text: "second"},
		{NodeID: "3", Kind: "text", Text: "42"},
		{NodeID: "3", Kind: "text", Text: "true"},
	}
	if len(cap.Outputs) != len(want) {
		t.Fatalf("outputs = %+v, want %+v", cap.Outputs, want)
	}
	for i, o := range cap.Outputs {
		if o.NodeID != want[i].NodeID || o.Kind != want[i].Kind || o.Text != want[i].Text || o.Filename != "" {
			t.Errorf("output[%d] = %+v, want %+v", i, o, want[i])
		}
	}
}

// TestComfy_ParseHistory_OutputOrderStable guards determinism: the history
// outputs map is unordered, so the sort must produce the same list every time
// (the UI keys and renders outputs in that order).
func TestComfy_ParseHistory_OutputOrderStable(t *testing.T) {
	const fixture = `{
	  "x": {
	    "prompt": [0, "x", {}, {}, []],
	    "outputs": {
	      "1": {"text": ["alpha"]},
	      "2": {"images": [{"filename": "b.png", "subfolder": "s", "type": "output", "metadata": {"kind": "image"}}]},
	      "3": {"images": [{"filename": "a.png", "subfolder": "", "type": "output", "metadata": {"kind": "image"}}]},
	      "4": {"text": ["beta", "gamma"]},
	      "5": {"string": ["delta"]}
	    },
	    "status": {"status_str": "success", "completed": true, "messages": []}
	  }
	}`
	// Files by filename first (a.png, b.png), then text results in node order.
	want := []string{"3/image/a.png/", "2/image/b.png/", "1/text//alpha", "4/text//beta", "4/text//gamma", "5/text//delta"}
	for i := 0; i < 20; i++ {
		cap, err := ParseHistory([]byte(fixture))
		if err != nil {
			t.Fatalf("ParseHistory (run %d): %v", i, err)
		}
		got := make([]string, 0, len(cap.Outputs))
		for _, o := range cap.Outputs {
			got = append(got, o.NodeID+"/"+o.Kind+"/"+o.Filename+"/"+o.Text)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d outputs = %v, want %d entries", i, got, len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("run %d output order = %v, want %v", i, got, want)
			}
		}
	}
}

// TestComfy_CaptureFromLive proves an in-progress queue item becomes a capture
// with its workflow, nodes and an empty output list.
func TestComfy_CaptureFromLive(t *testing.T) {
	live := &LivePrompt{
		Prompt:    json.RawMessage(`{"44":{"inputs":{"unet_name":"Anima/nova.safetensors"},"class_type":"UNETLoader","_meta":{"title":"Load Diffusion Model"}}}`),
		ExtraData: json.RawMessage(`{"extra_pnginfo":{"workflow":{"nodes":[{"id":44}]}}}`),
	}
	cap := CaptureFromLive("p1", live, StatusQueued)
	if cap == nil {
		t.Fatal("CaptureFromLive = nil")
	}
	if cap.PromptID != "p1" {
		t.Errorf("PromptID = %q", cap.PromptID)
	}
	if cap.Status.StatusStr != "queued" || cap.Status.Completed {
		t.Errorf("status = %+v, want queued/incomplete", cap.Status)
	}
	if cap.Outputs == nil || len(cap.Outputs) != 0 {
		t.Errorf("outputs = %#v, want a non-nil empty slice", cap.Outputs)
	}
	if len(cap.Nodes) != 1 || len(cap.Nodes[0].Models) != 1 || cap.Nodes[0].Models[0] != "Anima/nova.safetensors" {
		t.Errorf("nodes = %+v", cap.Nodes)
	}
	if len(cap.WorkflowAPI) == 0 || len(cap.WorkflowUI) == 0 {
		t.Errorf("workflows not captured: api=%d ui=%d", len(cap.WorkflowAPI), len(cap.WorkflowUI))
	}

	if got := CaptureFromLive("p1", nil, StatusQueued); got != nil {
		t.Errorf("CaptureFromLive(nil) = %+v, want nil", got)
	}
	if got := CaptureFromLive("p1", &LivePrompt{}, StatusQueued); got != nil {
		t.Errorf("CaptureFromLive(empty) = %+v, want nil", got)
	}
}
