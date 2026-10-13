package comfy

import (
	"encoding/json"
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
	cap := &Capture{Outputs: []Output{{Filename: "a.png", Type: "output"}, {Filename: "b.mp4", Subfolder: "s", Type: "output"}}}
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
