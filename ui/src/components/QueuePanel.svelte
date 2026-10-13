<script lang="ts">
  import type { QueueSnapshot } from "../lib/types";
  import { getQueue } from "../stores/api";
  import { connectionState } from "../stores/theme";
  import { formatDuration } from "../lib/format";
  import { Card } from "$lib/components/ui/card/index.js";

  let snapshot = $state<QueueSnapshot>({ queued: [], next_model: "" });
  let error = $state("");
  let refreshTimer: ReturnType<typeof setTimeout> | null = null;

  async function refresh() {
    try {
      snapshot = await getQueue();
      error = "";
    } catch (e) {
      error = e instanceof Error ? e.message : String(e);
    }
  }

  function scheduleRefresh() {
    if (refreshTimer !== null) return;
    refreshTimer = setTimeout(() => {
      refreshTimer = null;
      refresh();
      scheduleRefresh();
    }, 1500);
  }

  $effect(() => {
    if ($connectionState !== "connected") return;
    refresh();
    scheduleRefresh();
    return () => {
      if (refreshTimer !== null) clearTimeout(refreshTimer);
    };
  });
</script>

<Card class="p-4">
  <div class="mb-2 flex items-center justify-between">
    <h3 class="text-sm font-semibold">Request Queue</h3>
    <span class="text-xs text-muted-foreground">
      {#if snapshot.next_model}
        next: <span class="font-medium text-foreground">{snapshot.next_model}</span>
      {:else}
        idle
      {/if}
    </span>
  </div>

  {#if error}
    <p class="text-xs text-red-500">{error}</p>
  {:else if snapshot.queued.length === 0}
    <p class="text-xs text-muted-foreground">No queued requests</p>
  {:else}
    <table class="w-full text-left text-xs">
      <thead>
        <tr class="text-muted-foreground border-b">
          <th class="py-1 pr-2 font-medium">#</th>
          <th class="py-1 pr-2 font-medium">Model</th>
          <th class="py-1 pr-2 font-medium">Path</th>
          <th class="py-1 pr-2 font-medium">Waiting</th>
        </tr>
      </thead>
      <tbody>
        {#each snapshot.queued as item (item.position + item.model + item.path)}
          <tr class="border-b border-white/5" class:opacity-70={!item.next_swap_for}>
            <td class="py-1 pr-2">{item.position}</td>
            <td class="py-1 pr-2 font-medium">
              {item.model}
              {#if item.next_swap_for}
                <span class="ml-1 rounded bg-amber-500/15 px-1 text-[10px] text-amber-600 dark:text-amber-400">next</span>
              {/if}
            </td>
            <td class="py-1 pr-2 font-mono">{item.path}</td>
            <td class="py-1 pr-2">{item.waiting_ms > 0 ? formatDuration(item.waiting_ms) : "-"}</td>
          </tr>
        {/each}
      </tbody>
    </table>
  {/if}
</Card>
