<script lang="ts">
	import { countChanges, diffLines, foldContext } from '$lib/agent/diff';
	import { m } from '$lib/paraglide/messages';

	interface Props {
		before: string;
		after: string;
	}

	let { before, after }: Props = $props();

	const full = $derived(diffLines(before, after));
	const counts = $derived(countChanges(full));
	const lines = $derived(foldContext(full));
</script>

<div data-slot="agent-diff" class="overflow-hidden rounded-lg border">
	<p class="border-b bg-muted/50 px-3 py-1 text-xs text-muted-foreground tabular-nums">
		<span>+{counts.added}</span>
		<span aria-hidden="true">&nbsp;</span>
		<span>−{counts.removed}</span>
	</p>
	<pre class="max-h-80 overflow-auto py-1 text-xs leading-5"><code
			>{#each lines as line, index (index)}{#if line.kind === 'gap'}<span
						class="block px-3 text-muted-foreground">{m.agent_diff_unchanged({ count: line.hidden })}</span
					>{:else if line.kind === 'add'}<span
						class="block min-w-fit bg-success px-3 text-success-foreground"
						><span class="sr-only">{m.agent_diff_added()}: </span><span aria-hidden="true">+ </span
						>{line.text === '' ? ' ' : line.text}</span
					>{:else if line.kind === 'remove'}<span
						class="block min-w-fit bg-action-destructive px-3 text-action-destructive-foreground"
						><span class="sr-only">{m.agent_diff_removed()}: </span><span aria-hidden="true"
							>− </span
						>{line.text === '' ? ' ' : line.text}</span
					>{:else}<span class="block min-w-fit px-3"
						><span aria-hidden="true">&nbsp;&nbsp;</span>{line.text === '' ? ' ' : line.text}</span
					>{/if}{/each}</code
		></pre>
</div>
