<script lang="ts">
  interface Props {
    statusCode: number;
    metadata: Record<string, string> | undefined;
  }

  let { statusCode, metadata }: Props = $props();

  const COMFY_STATUS = "comfy_status";

  // Badge classes keyed by ComfyUI workflow terminal state. The running state
  // is shown while the workflow is still executing.
  const styles: Record<string, string> = {
    running: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
    completed: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
    failed: "bg-red-500/15 text-red-600 dark:text-red-400",
    cancelled: "bg-zinc-500/15 text-zinc-600 dark:text-zinc-400",
    unknown: "bg-zinc-500/15 text-zinc-500 dark:text-zinc-400",
  };

  let comfyStatus = $derived(metadata?.[COMFY_STATUS] ?? "");
  let badgeClass = $derived(styles[comfyStatus] ?? styles.unknown);
</script>

{#if comfyStatus}
  <span class="inline-flex items-center rounded px-1.5 py-0.5 text-xs font-medium {badgeClass}">
    {comfyStatus}
  </span>
{:else}
  <span>{statusCode || "-"}</span>
{/if}
