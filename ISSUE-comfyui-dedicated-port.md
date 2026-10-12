<!--
  GitHub issue for mostlygeek/llama-swap
  Title: ComfyUI endpoint (/comfyui/) cannot be used by ComfyUI clients that require the API at the path root (MCP server, comfy-cli)
-->

# ComfyUI endpoint (/comfyui/) cannot be used by ComfyUI clients that require the API at the path root (MCP server, comfy-cli)

### Summary

The `comfyui_auto` model is served at `/comfyui/`, i.e. under a
subdirectory of the main llama-swap port. ComfyUI clients that speak the
ComfyUI API - the ComfyUI MCP server and `comfy-cli` for example - are
configured with a single base URL (`http://host:port`) and issue requests
to root-level paths (`GET /`, `POST /api/prompt`, `/ws`, ...). They cannot
address a subdirectory, so the `/comfyui/` endpoint is not usable from
these tools as it stands.

### Additional limitation

On the main port, only the exact root path `/comfyui/` may start an
unloaded `comfyui_auto` model; any other path under `/comfyui/` (e.g.
`/comfyui/api/prompt`) is refused with a `409` while the model is not
loaded. I understand this guard exists for the browser UI (stale asset or
websocket requests after an unload must not restart the model).

However, ComfyUI MCP servers and comfy-cli do not request the root path
first: the first request they make against an unloaded model is an API
path (`POST /api/prompt`, `GET /system_stats`, `/ws`, ...), not `/`. So
even apart from the subdirectory problem, the root-only start rule is a
second obstacle for these tools: until something else has loaded the
model, every request they make is refused with a `409`.

### Request

An option to additionally serve `comfyui_auto` on a dedicated port at the
path root, for example:

```yaml
comfyui:
  port: 8188   # 0 (default) disables the dedicated listener
```

Expected behaviour on the dedicated port:

- the model is served at the path root, so `COMFYUI_URL=http://host:8188`
  works with `comfy-cli` and the ComfyUI MCP server;
- any request path may start the unloaded model, because the tools above
  never request the root path first - without this, their very first
  request would be refused;
- websocket handling still follows the model
  `compat.ignoreWebsockets` (forced on for `comfyui_auto`): an upgrade
  while the model is unloaded returns `409`, and websockets never start,
  keep alive, or swap the model;
- the configured port is rejected at config load when it collides with the
  main `-listen` port or a port allocated to a model via `${PORT}`.

The existing `/comfyui/` endpoint on the main port would remain unchanged.

### Environment

- llama-swap: v256
- setup: llama-swap proxies vLLM/ninfer LLM backends and ComfyUI
  (`comfyui_auto`) for a single GPU machine; ComfyUI MCP / comfy-cli
  currently have no base URL they can point at.
