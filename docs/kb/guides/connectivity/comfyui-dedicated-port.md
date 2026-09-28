---
title: ComfyUI endpoint and dedicated port
summary: Serve ComfyUI clients through /comfyui/ or a dedicated root-port listener for MCP and CLI tools that cannot use a subdirectory.
category: guides
tags: [comfyui, port, mcp, websocket]
config_keys: [comfyui.port, models.*.compat.ignoreWebsockets]
updated: 2026-09-28
---

# ComfyUI endpoint and dedicated port

llama-swap serves the fixed `comfyui_auto` model at `/comfyui/`. ComfyUI MCP
servers and comfy-cli address the ComfyUI API at the path root and cannot use a
subdirectory. `comfyui.port` opens a second HTTP listener (bound on all
interfaces) that serves the same model at the root, so those clients can point
at `http://host:<port>` directly:

```yaml
models:
  comfyui_auto:
    cmd: python main.py --listen --port ${PORT}
    checkEndpoint: /system_stats

comfyui:
  port: 8188   # 0 (the default) disables the listener
```

## Start gating

The dedicated port applies the same start gating as `/comfyui/` (see
`guides/upstreams/comfyui` for the full list): while `comfyui_auto` is not
ready, a `GET` to an ignored path is answered with `409 Conflict` and does not
start the model, with the message `model comfyui_auto is not loaded; ignored
ComfyUI paths cannot start it`. Anything else starts the model as usual: a
write (a job queueing `POST`) on any path, the page itself, `/api/prompt`, and
every other path not in the ignore list.

The ignored paths are the same fixed set as the `/comfyui/` endpoint: static
assets, `/ws/...`, and `/api/jobs`.

## Websockets and auth

Websocket handling follows `compat.ignoreWebsockets` (forced on for
`comfyui_auto`): a websocket upgrade never starts the model and an unloaded
model answers one with 409. Open the ComfyUI UI or any plain HTTP path first
so the model is loaded before the websocket connects.

API keys, request logging, and CORS apply on the dedicated port exactly as on
`/comfyui/`.

What goes wrong:

- **Port in use at startup**: llama-swap exits with an error (like the main
  listener and Tailcat). Pick a free port.
- **Port in use after a reload**: the reload warns and keeps the previous
  listener; a port change on reload restarts the listener.
- **`comfyui.port` equals the main `-listen` port, or a `${PORT}` allocated
  to a model**: rejected at config load with a conflict error.
- **`404 local model comfyui_auto not found`**: the dedicated listener is
  bound to the fixed model; define `models.comfyui_auto` first.
- **Everything works except the websocket**: expected until a non-websocket
  request has loaded the model (see above).
- **`409` on a saved workflow**: workflow files end in `.json`, which the
  static-asset pattern covers, so fetching one cannot start the model. Load
  the root path first.
