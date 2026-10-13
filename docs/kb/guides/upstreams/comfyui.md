---
title: ComfyUI with the /comfyui endpoint
summary: Name a model comfyui_auto and use /comfyui/ so ComfyUI's websocket does not block model swapping.
category: guides
tags: [comfyui, image, websocket, upstream, swapping, ttl, docker]
config_keys: [comfyui.swapMode, comfyui.vramFreeFraction, comfyui.queueTimeout, comfyui.startEvicts, models.*.compat.ignoreWebsockets, models.*.concurrencyLimit, models.*.checkEndpoint, models.*.cmdStop, models.*.unloadTimeout, models.*.ttl, models.*.unlisted, upstream.ignorePaths]
updated: 2026-10-12
---

# ComfyUI with the /comfyui endpoint

ComfyUI is a web app, not an OpenAI-compatible server, so it does not swap well
through the normal endpoints. Its browser frontend holds a websocket open for
as long as the tab is open. Counted as a normal request, that connection makes
the model look permanently busy and llama-swap will not swap it out.

`/comfyui/` is a dedicated passthrough that applies the needed settings for
you. It is wired to one reserved model ID: `comfyui_auto`.

## Setup

Name the model exactly `comfyui_auto` and point a browser at
`http://localhost:8080/comfyui/`.

```yaml
models:
  comfyui_auto:
    # ComfyUI has no /health; its root returns 200 once the server is up
    checkEndpoint: /
    cmdStop: docker stop comfyui-auto
    cmd: >
      docker run --rm
      --name comfyui-auto --runtime=nvidia
      --gpus '"device=2,3"' -p ${PORT}:8188
      -v /path/to/comfyui/storage-cache/dot-cache:/root/.cache
      -v /path/to/comfyui/storage-cache/dot-config:/root/.config
      -v /path/to/comfyui/storage-nodes/dot-local:/root/.local
      -v /path/to/comfyui/storage-nodes/custom_nodes:/root/ComfyUI/custom_nodes
      -v /path/to/comfyui/storage-models/models:/root/ComfyUI/models
      -v /path/to/comfyui/storage-models/hf-hub:/root/.cache/huggingface/hub
      -v /path/to/comfyui/storage-models/torch-hub:/root/.cache/torch/hub
      -v /path/to/comfyui/storage-user/input:/root/ComfyUI/input
      -v /path/to/comfyui/storage-user/output:/root/ComfyUI/output
      -v /path/to/comfyui/storage-user/user-profile:/root/ComfyUI/user
      -v /path/to/comfyui/storage-user/user-scripts:/root/user-scripts
      -e CLI_ARGS=""
      yanwk/comfyui-boot:cu130-slim-v2
    unloadTimeout: 30   # docker stop is slow
    ttl: 600
    unlisted: true      # it is not an OpenAI model, keep it out of /v1/models
```

`cmdStop` matters here: without it llama-swap stops the local `docker run`
client and the container keeps the GPU. See
`guides/model-runtime/ttl-and-unloading`.

Nothing else is required. `/comfyui` (no trailing slash) redirects to
`/comfyui/` and keeps the query string, so `?token=...` style links survive.
API key checks apply exactly as they do on other endpoints.

## What llama-swap forces on comfyui_auto

Two settings are applied while the config loads:

| setting | value | why |
| --- | --- | --- |
| `compat.ignoreWebsockets` | forced to `true` | a websocket connection does not start or queue the model, and does not count toward concurrency, in-flight requests, TTL activity, or swap decisions |
| `concurrencyLimit` | raised to at least `50` | the per-model default is 10; one open ComfyUI tab makes many parallel asset and API requests |

Setting `ignoreWebsockets: false` yourself has no effect, and a
`concurrencyLimit` below 50 is raised. A higher one is kept.

## What cannot start the model

The ComfyUI frontend retries its websocket for as long as the tab is open, so
after an unload it would pull the model back in on its own and never give up the
GPU. Two separate rules prevent that. Both answer `409 Conflict` while the model
is not ready, with different messages:

| request | message | comes from |
| --- | --- | --- |
| a `GET` to an ignored path | `ignored ComfyUI paths cannot start it` | the `/comfyui/` endpoint |
| a websocket upgrade, on any path | `ignored websocket requests cannot start it` | `compat.ignoreWebsockets` |

The second rule is what `ignoreWebsockets` does everywhere, not something
`/comfyui/` adds; it also keeps the connection out of the scheduler entirely.
The endpoint's own rule matches on the path alone, so it also covers a plain
`GET` to `/ws` that carries no upgrade headers.

The ignored paths are fixed. They are the same set you would configure by hand
to serve ComfyUI through `/upstream/` instead:

| pattern | covers |
| --- | --- |
| `.*\.(js\|json\|css\|png\|gif\|jpg\|jpeg\|ico\|txt)$` | static assets, the shared `upstream.ignorePaths` default |
| `^/ws(/\|$)` | the frontend websocket, and any path under it |
| `^/api/jobs$` | the job list the open UI polls |

Only a `GET` is ignored. A write is a deliberate action even on an ignored path,
so queueing a job with a `POST` still starts the model. Everything else starts it
as usual: the page itself, `/api/prompt`, and any path not in the list.

Query parameters are not part of the match, so `?clientId=...` makes no
difference. `/wsapi` and `/api/jobs/12` are different paths and are not ignored.

## What swaps and what does not

**Only a workflow submit (`POST /prompt`) may swap another model out.** Every
other ComfyUI request — reads, settings writes, uploads, `/interrupt`, `/free`,
job cancels, static assets — is served straight to ComfyUI and **never** evicts
a running LLM. So opening the history or asset panel cannot unload a model.

Those direct requests may still **start** ComfyUI if it is not running: they hold
the request until ComfyUI is ready, then serve it. Starting ComfyUI needs only a
small VRAM headroom (~500–700MiB), so by default it starts into free memory
without evicting anything (`comfyui.startEvicts: false`). Set `startEvicts: true`
only if a start should reclaim a sibling model's GPU when headroom is tight.

- **`/comfyui/`** keeps its start-gating: a `GET` to an ignored path (static
  assets, `/ws`, `/api/jobs`) cannot start a cold ComfyUI. Other requests start it.
- **`comfyui.port`** (dedicated listener) cold-starts ComfyUI on **any** request.

`comfyui.queueTimeout` bounds how long a `POST /prompt` may wait in the queue
before it is rejected with `503` (default `0` = wait forever).

```yaml
comfyui:
  port: 8188
  swapMode: cleanup
  vramFreeFraction: 0.02
  queueTimeout: 300
  startEvicts: false
```

Set `comfyui.swapMode` to control what "swapping ComfyUI out" means when a
`POST /prompt` (or another model) needs the GPU:

| swapMode | swap-out behaviour |
| --- | --- |
| `unload` (default) | stop the ComfyUI process, like every other model |
| `cleanup` | keep the process running and only free its GPU memory (`POST /free`), so the next swap-in is fast |

In `cleanup` mode a swap-out waits for ComfyUI's own running workflows to finish,
then frees its memory and keeps the process warm. TTL expiry and an explicit
unload (`/unload`) still stop the process even in `cleanup` mode.

The memory free is issued as soon as the workflow queue is empty — llama-swap
does **not** wait for VRAM to drop first (a finished workflow keeps its memory
until the free). After the free it briefly confirms VRAM was released, then
proceeds regardless. The free and all status probes go directly to ComfyUI's own
port, so they are never blocked by the swap they are unblocking.

`comfyui.vramFreeFraction` is the fraction of device VRAM that may still be in
use and still count as memory-free (ComfyUI keeps a small CUDA context resident
even after freeing). The default `0.02` tolerates that resident context.

**ComfyUI must listen on `127.0.0.1`** so every submit goes through llama-swap's
queue. A client that reaches ComfyUI's own port directly bypasses the queue and
can start a workflow while a mutually exclusive model holds the GPU.

## Task visibility

Two places show ComfyUI workflow tasks:

- **Activity panel → ComfyUI Tasks**: live queued/running prompts plus recent
  finished ones, each with its state (`queued`, `running`, `completed`,
  `failed`, `cancelled`) and duration.
- **`GET /api/comfy/jobs`**: the same merged list as JSON — live prompts from
  ComfyUI's queue plus terminal states recorded by the task tracker.

Each submitted workflow also appears as a row in the main Activity table (`model`
`comfyui_auto`, path `/prompt`); its Status column shows the workflow's terminal
state and the row is updated in place when it finishes. A workflow not observed
within the tracking window is marked `unknown`.

## Request queue visibility

The Activity panel also shows a **Request Queue** view: the requests waiting in
llama-swap's FIFO scheduler, in order, each with its model, request path and
wait time, and which model the head of the queue would swap in next. It answers
"what is queued and what loads next". The same data is available as JSON at
`GET /api/queue`. This is a read-only view — it never affects scheduling.

## What goes wrong

- **`404 local model comfyui_auto not found`.** The model is named something
  else, or it is defined on a peer. `/comfyui/` only serves a local model with
  that exact ID. Aliases do not work here — the lookup is by model ID.
- **A stale tab reports a lost connection.** The model unloaded while the tab
  was open and its websocket now gets one of the 409s above. Interacting with
  the page starts the model again; the websocket reconnects once it is ready.
- **An idle tab keeps the model loaded.** Anything the page polls that is not
  in the list above still counts as a real request and can reload the model
  after a TTL unload. Close the tab, or route that instance through `/upstream/`
  with an `upstream.ignorePaths` entry for the path it polls.
- **Opening a saved workflow returns 409.** Workflow files end in `.json`, which
  the static-asset pattern covers, so fetching one cannot start the model. Load
  `/comfyui/` first.
- **The model unloads while you are working.** Websocket traffic is ignored, so
  it does not reset the TTL timer. Watching a long render over the websocket
  counts as idle. Use a longer `ttl`, or `ttl: 0` to disable automatic
  unloading, and rely on a swap to free the VRAM.
- **ComfyUI never becomes ready.** The default `checkEndpoint` is `/health`,
  which ComfyUI does not serve. Set `checkEndpoint: /`, or `none` to skip the
  check. See `guides/model-runtime/troubleshooting-model-wont-load`.

## If you cannot use comfyui_auto

Only one ComfyUI instance can use `/comfyui/`. For a second one, or to keep
your own model ID, serve it through `/upstream/<model>/` and reproduce the
settings by hand:

```yaml
models:
  my-other-comfyui-model:
    checkEndpoint: /
    # the two settings /comfyui/ would have applied for you
    concurrencyLimit: 50
    compat:
      ignoreWebsockets: true
    cmdStop: docker stop ${MODEL_ID}
    cmd: >
      docker run --rm
      --name ${MODEL_ID} --runtime=nvidia
      --gpus '"device=2,3"' -p ${PORT}:8188
      -v /path/to/comfyui/storage-models/models:/root/ComfyUI/models
      -v /path/to/comfyui/storage-user/output:/root/ComfyUI/output
      -e CLI_ARGS=""
      yanwk/comfyui-boot:cu130-slim-v2

upstream:
  ignorePaths:
    # the same three patterns /comfyui/ applies on its own
    - '.*\.(js|json|css|png|gif|jpg|jpeg|ico|txt)$'
    - ^\/ws(\/|$)
    - ^\/api\/jobs$
```

The mounts are trimmed here; copy the full set from the block above. `cmd` is a
folded YAML scalar, so a `#` line inside it is an argument, not a comment.

Use `${MODEL_ID}` for the container name so a second instance cannot collide
with the `comfyui-auto` container, and keep the default static-asset pattern —
listing `ignorePaths` at all replaces the default instead of adding to it.

`compat.ignoreWebsockets` does the same job here as it does on `/comfyui/`.
`upstream.ignorePaths` stands in for the endpoint's built-in list, which applies
to `/comfyui/` only, and is also where you add paths that list does not cover.
Two differences: listing any pattern replaces the static-asset default, so
repeat it, and `ignorePaths` matches every method rather than `GET` alone.

## Related

- `guides/connectivity/upstream-passthrough` — the general passthrough endpoint
- `guides/model-runtime/ttl-and-unloading` — TTL and unload behaviour
- `guides/routing/capacity-and-queues` — what `concurrencyLimit` does
