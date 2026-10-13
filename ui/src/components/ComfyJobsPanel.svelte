<script lang="ts">
  import type { ComfyJob } from "../lib/types";
  import { getComfyJobs } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { formatDuration } from "../lib/format";
  import { Card } from "$lib/components/ui/card/index.js";

  let jobs = $state<ComfyJob[]>([]);
  let error = $state("");
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;

  const statusStyles: Record<string, string> = {
    queued: "bg-blue-500/15 text-blue-600 dark:text-blue-400",
    running: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
    completed: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
    failed: "bg-red-500/15 text-red-600 dark:text-red-400",
    cancelled: "bg-zinc-500/15 text-zinc-600 dark:text-zinc-400",
    unknown: "bg-zinc-500/15 text-zinc-500 dark:text-zinc-400",
  };

  async function refresh() {
    try {
      jobs = await getComfyJobs();
      error = "";
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  // Poll gently; the list is small and there is no SSE channel for ComfyUI jobs
  // yet. Live rows (queued/running) are what the operator watches for.
  function scheduleRefresh() {
    if (refreshTimer !== null) return;
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refresh();
      scheduleRefresh();
    }, 2000);
  }

  $effect(() => {
    if ($connectionState !== "connected") return;
    refresh();
    scheduleRefresh();
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
    };
  });

  function badge(job: ComfyJob): string {
    return statusStyles[job.status] ?? statusStyles.unknown;
  }

  let liveJobs = $derived(jobs.filter((j) => j.live || j.status === "queued" || j.status === "running"));
  let recentJobs = $derived(jobs.filter((j) => !j.live && j.status !== "queued" && j.status !== "running"));
</script>

<Card class="p-4">
  <div class="mb-2 flex items-center justify-between">
    <h3 class="text-sm font-semibold">ComfyUI Tasks</h3>
    <span class="text-xs text-muted-foreground">{liveJobs.length} active</span>
  </div>

  {#if error}
    <p class="text-xs text-red-500">{error}</p>
  {:else if jobs.length === 0}
    <p class="text-xs text-muted-foreground">No ComfyUI tasks</p>
  {:else}
    <table class="w-full text-left text-xs">
      <thead>
        <tr class="text-muted-foreground border-b">
          <th class="py-1 pr-2 font-medium">Prompt</th>
          <th class="py-1 pr-2 font-medium">Status</th>
          <th class="py-1 pr-2 font-medium">Duration</th>
        </tr>
      </thead>
      <tbody>
        {#each liveJobs as job (job.prompt_id)}
          <tr class="border-b border-white/5">
            <td class="py-1 pr-2 font-mono">{job.prompt_id.slice(0, 8)}</td>
            <td class="py-1 pr-2">
              <span class="inline-flex items-center rounded px-1.5 py-0.5 font-medium {badge(job)}">{job.status}</span>
            </td>
            <td class="py-1 pr-2">{job.duration_ms > 0 ? formatDuration(job.duration_ms) : "-"}</td>
          </tr>
        {/each}
        {#each recentJobs as job (job.prompt_id)}
          <tr class="border-b border-white/5 opacity-70">
            <td class="py-1 pr-2 font-mono">{job.prompt_id.slice(0, 8)}</td>
            <td class="py-1 pr-2">
              <span class="inline-flex items-center rounded px-1.5 py-0.5 font-medium {badge(job)}">{job.status}</span>
            </td>
            <td class="py-1 pr-2">{job.duration_ms > 0 ? formatDuration(job.duration_ms) : "-"}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Card>
