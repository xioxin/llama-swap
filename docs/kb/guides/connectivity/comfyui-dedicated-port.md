---
title: ComfyUI endpoint and dedicated port
summary: Serve ComfyUI clients through /comfyui/ or a dedicated root-port listener for MCP and CLI tools that cannot use a subdirectory.
category: guides
tags: [comfyui, port, mcp, websocket]
config_keys: [comfyui.port, models.*.compat.ignoreWebsockets]
updated: 2026-09-19
---

# ComfyUI endpoint and dedicated port

llama-swap serves the fixed `comfyui_auto` model at `/comfyui/`, where only
the root path may start an unloaded model: stale browser requests for
assets, APIs, or websockets must not reload the process.

ComfyUI MCP servers and comfy-cli address the ComfyUI API at the path root
and cannot use a subdirectory. `comfyui.port` opens a second HTTP listener
(bound on all interfaces) that serves the same model at the root, so those
clients can point at `http://host:<port>` directly:

```yaml
models:
  comfyui_auto:
    cmd: python main.py --listen --port ${PORT}
    checkEndpoint: /system_stats

comfyui:
  port: 8188   # 0 disables the listener
```

Unlike `/comfyui/`, any path on the dedicated listener may start the model,
including `GET /` and `/api/prompt`. Point ComfyUI clients at the listener
base URL, for example `COMFYUI_URL=http://host:8188` for comfy-cli.

Websocket handling still follows the model
`compat.ignoreWebsockets` (forced on for `comfyui_auto`): an unloaded model
answers a websocket upgrade with 409 and websockets never start, swap, or
keep the process alive. Open the ComfyUI UI (or any plain HTTP path) first
so the model is loaded before the websocket connects.

What goes wrong:

- **Port in use at startup**: llama-swap exits with an error (like the main
  listener and Tailcat). Pick a free port.
- **Port in use after a reload**: the reload warns and keeps the previous
  listener; a port change on reload restarts the listener.
- **`comfyui.port` equals the main `-listen` port, or a `${PORT}` allocated
  to a model**: rejected at config load with a conflict error.
- **404 `local model comfyui_auto not found`**: the dedicated listener is
  bound to the fixed model; define `models.comfyui_auto` first.
- **Everything works except the websocket**: expected until a non-websocket
  request has loaded the model (see above).

