<script lang="ts">
  import type { ComfyCapture, ComfyOutput } from "../lib/types";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { copyText } from "../lib/clipboard";
  import { formatDuration } from "../lib/format";
  import { models } from "../stores/api";

  interface Props {
    capture: ComfyCapture | null;
    open: boolean;
    onclose: () => void;
  }

  let { capture, open, onclose }: Props = $props();

  let contentEl = $state<HTMLElement | null>(null);
  let copiedWorkflow = $state(false);

  type Tab = "outputs" | "workflow" | "details";
  let tab: Tab = $state("outputs");

  // previewLoaded tracks, per output filename, whether to load its media. When
  // ComfyUI is already running, previews load immediately (no placeholder); a
  // cold ComfyUI shows a placeholder so opening the dialog does not wake it —
  // only an explicit click does.
  let previewLoaded = $state<Record<string, boolean>>({});

  // comfyReady mirrors the comfyui_auto process state from the model feed. The
  // fixed ComfyUI model id is "comfyui_auto".
  let comfyReady = $derived(
    ($models ?? []).some((m) => m.id === "comfyui_auto" && m.state === "ready"),
  );

  // When ComfyUI is (or becomes) running, auto-load every preview.
  $effect(() => {
    if (!comfyReady) return;
    for (const o of capture?.outputs ?? []) {
      if (previewable(o)) previewLoaded[o.filename] = true;
    }
  });

  function pretty(v: unknown): string {
    if (v == null) return "";
    if (typeof v === "string") return v;
    try {
      return JSON.stringify(v, null, 2);
    } catch {
      return String(v);
    }
  }

  function workflowUIJson(c: ComfyCapture): string {
    return pretty(c.workflow_ui);
  }
  function workflowAPIJson(c: ComfyCapture): string {
    return pretty(c.workflow_api);
  }

  function downloadJson(name: string, content: string) {
    const blob = new Blob([content], { type: "application/json" });
    const a = document.createElement("a");
    a.href = URL.createObjectURL(blob);
    a.download = name;
    a.click();
    URL.revokeObjectURL(a.href);
  }

  async function copyWorkflow(text: string) {
    if (!(await copyText(text, contentEl))) return;
    copiedWorkflow = true;
    setTimeout(() => (copiedWorkflow = false), 1500);
  }

  function previewable(o: ComfyOutput): boolean {
    return o.kind === "image" || o.kind === "video";
  }
  function isImage(o: ComfyOutput): boolean {
    return o.kind === "image";
  }
  function isVideo(o: ComfyOutput): boolean {
    return o.kind === "video";
  }

  function dims(o: ComfyOutput): string {
    if (!o.width && !o.height) return "";
    return `${o.width || "?"}×${o.height || "?"}`;
  }
  function mediaInfo(o: ComfyOutput): string {
    const bits: string[] = [];
    const d = dims(o);
    if (d) bits.push(d);
    if (o.fps) bits.push(`${Math.round(o.fps)} fps`);
    if (o.duration) bits.push(formatDuration(Math.round(o.duration * 1000)));
    if (o.frame_count) bits.push(`${o.frame_count} frames`);
    return bits.join(" · ");
  }

  let modelList = $derived.by(() => {
    const set = new Set<string>();
    for (const n of capture?.nodes ?? []) for (const m of n.models ?? []) set.add(m);
    return [...set].sort();
  });

  // Outputs and nodes may be null/absent (a task cancelled before it ran, or a
  // snapshot with no graph). Normalise to arrays so the template never reads
  // .length or iterates over null.
  let outputs = $derived(capture?.outputs ?? []);
  let nodeList = $derived(capture?.nodes ?? []);
  let comfyStatus = $derived(capture?.status ?? { status_str: "", completed: false, started_at_ms: 0, finished_at_ms: 0, duration_ms: 0, cached_nodes: 0 });
</script>

<Dialog.Root
  {open}
  onOpenChange={(v) => {
    if (!v) onclose();
  }}
>
  <Dialog.Content
    bind:ref={contentEl}
    class="flex max-h-[90vh] w-[90%] sm:max-w-[90%] flex-col gap-0 p-0"
  >
    {#if capture}
      <Dialog.Header class="border-b border-border px-4 py-3">
        <Dialog.Title class="flex items-center gap-2 text-lg font-bold">
          ComfyUI Workflow
          <span class="font-mono text-sm font-normal text-muted-foreground">{capture.prompt_id}</span>
          <span class="rounded px-1.5 py-0.5 text-xs {comfyStatus.completed ? 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400' : 'bg-red-500/15 text-red-600 dark:text-red-400'}">
            {comfyStatus.status_str || (comfyStatus.completed ? "completed" : "incomplete")}
          </span>
        </Dialog.Title>
      </Dialog.Header>

      <!-- Tabs -->
      <div class="flex gap-1 border-b border-border px-4 pt-2">
        {#each ([["outputs", `Outputs (${outputs.length})`], ["workflow", "Workflow"], ["details", "Details"]] as [Tab, string][]) as [id, label]}
          <button
            class="rounded-t px-3 py-1.5 text-sm {tab === id ? 'border-b-2 border-primary font-medium text-primary' : 'text-muted-foreground hover:text-foreground'}"
            onclick={() => (tab = id)}
          >
            {label}
          </button>
        {/each}
      </div>

      <div class="min-h-0 flex-1 overflow-y-auto p-4">
        {#if tab === "outputs"}
          {#if outputs.length === 0}
            <p class="text-sm text-muted-foreground">No output files recorded.</p>
          {:else}
            <div class="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
              {#each outputs as o (o.node_id + o.filename)}
                <div class="rounded-md border border-border overflow-hidden">
                  <div class="flex items-center justify-center bg-muted/30 aspect-video">
                    {#if !previewable(o)}
                      <!-- Unknown type: download only. -->
                      <div class="p-4 text-center">
                        <div class="text-2xl">📄</div>
                        <div class="mt-1 text-xs text-muted-foreground">{o.kind || "file"}</div>
                      </div>
                    {:else if !previewLoaded[o.filename]}
                      <div class="p-4 text-center">
                        <div class="text-2xl">{isVideo(o) ? "🎬" : "🖼️"}</div>
                        <div class="mt-1 text-xs text-muted-foreground">preview not loaded</div>
                        <Button size="sm" variant="outline" class="mt-2" onclick={() => (previewLoaded[o.filename] = true)}>
                          Load preview
                        </Button>
                      </div>
                    {:else if isImage(o)}
                      <img src={o.url} alt={o.filename} class="max-h-full w-full object-contain" loading="lazy" />
                    {:else}
                      <video src={o.url} controls class="max-h-full w-full">
                        <track kind="captions" />
                      </video>
                    {/if}
                  </div>
                  <div class="space-y-1 p-2">
                    <div class="truncate font-mono text-xs" title={o.filename}>{o.filename}</div>
                    {#if mediaInfo(o)}
                      <div class="text-xs text-muted-foreground">{mediaInfo(o)}</div>
                    {/if}
                    <div class="flex flex-wrap gap-1 pt-1">
                      {#if o.download_url}
                        <a href={o.download_url} class="inline-flex items-center rounded border border-border px-2 py-0.5 text-xs hover:bg-muted" download>
                          Download
                        </a>
                      {/if}
                      {#if previewable(o) && !previewLoaded[o.filename]}
                        <button class="inline-flex items-center rounded border border-border px-2 py-0.5 text-xs hover:bg-muted" onclick={() => (previewLoaded[o.filename] = true)}>
                          Preview
                        </button>
                      {/if}
                    </div>
                  </div>
                </div>
              {/each}
            </div>
          {/if}
        {:else if tab === "workflow"}
          <div class="space-y-3">
            <div class="flex flex-wrap gap-2">
              <Button size="sm" variant="outline" onclick={() => downloadJson("workflow.json", workflowUIJson(capture))}>
                Download UI workflow
              </Button>
              <Button size="sm" variant="outline" onclick={() => downloadJson("workflow-api.json", workflowAPIJson(capture))}>
                Download API workflow
              </Button>
              <Button size="sm" variant="ghost" onclick={() => copyWorkflow(workflowUIJson(capture))}>
                {copiedWorkflow ? "Copied" : "Copy UI workflow"}
              </Button>
            </div>
            <p class="text-xs text-muted-foreground">
              The UI workflow reopens in ComfyUI's editor; the API workflow is the executable node graph.
            </p>
            <pre class="max-h-[50vh] overflow-auto rounded-md border border-border bg-background p-3 text-xs">{workflowUIJson(capture)}</pre>
          </div>
        {:else}
          <!-- Details -->
          <div class="space-y-4">
            <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
              <div><div class="text-xs text-muted-foreground">Duration</div><div class="text-sm">{comfyStatus.duration_ms ? formatDuration(comfyStatus.duration_ms) : "-"}</div></div>
              <div><div class="text-xs text-muted-foreground">Status</div><div class="text-sm">{comfyStatus.status_str || "-"}</div></div>
              <div><div class="text-xs text-muted-foreground">Cached nodes</div><div class="text-sm">{comfyStatus.cached_nodes}</div></div>
              <div><div class="text-xs text-muted-foreground">Nodes</div><div class="text-sm">{nodeList.length}</div></div>
            </div>

            {#if modelList.length > 0}
              <div>
                <div class="mb-1 text-xs font-semibold uppercase tracking-wider text-muted-foreground">Models</div>
                <div class="flex flex-wrap gap-1">
                  {#each modelList as m}
                    <span class="rounded bg-muted px-1.5 py-0.5 font-mono text-xs">{m}</span>
                  {/each}
                </div>
              </div>
            {/if}

            <div>
              <div class="mb-1 text-xs font-semibold uppercase tracking-wider text-muted-foreground">Nodes</div>
              <div class="max-h-64 overflow-auto rounded-md border border-border">
                <table class="w-full text-xs">
                  <thead><tr class="border-b border-border text-muted-foreground"><th class="px-2 py-1 text-left">ID</th><th class="px-2 py-1 text-left">Type</th><th class="px-2 py-1 text-left">Title</th></tr></thead>
                  <tbody>
                    {#each nodeList as n (n.node_id)}
                      <tr class="border-b border-border last:border-0">
                        <td class="px-2 py-1 font-mono">{n.node_id}</td>
                        <td class="px-2 py-1 font-mono">{n.class_type}</td>
                        <td class="px-2 py-1">{n.title || "-"}</td>
                      </tr>
                    {/each}
                  </tbody>
                </table>
              </div>
            </div>
          </div>
        {/if}
      </div>
    {/if}
  </Dialog.Content>
</Dialog.Root>
