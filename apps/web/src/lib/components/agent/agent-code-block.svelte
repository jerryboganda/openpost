<script lang="ts">
	import CopyButton from '$lib/components/copy-button.svelte';
	import { m } from '$lib/paraglide/messages';

	interface Props {
		code: string;
		lang?: string;
		/** The fence is still open, so the block is not final yet. */
		streaming?: boolean;
	}

	let { code, lang = '', streaming = false }: Props = $props();

	const isDiff = $derived(lang === 'diff' || lang === 'patch');
	const lines = $derived(isDiff ? code.split('\n') : []);

	function lineClass(line: string): string {
		if (line.startsWith('+') && !line.startsWith('+++')) return 'bg-success text-success-foreground';
		if (line.startsWith('-') && !line.startsWith('---')) {
			return 'bg-action-destructive text-action-destructive-foreground';
		}
		return '';
	}
</script>

<div class="overflow-hidden rounded-lg border bg-muted/40" data-slot="agent-code">
	<div
		class="flex min-h-8 items-center justify-between gap-2 border-b bg-muted/60 pr-1 pl-3 text-xs text-muted-foreground"
	>
		<span class="truncate">{lang || m.agent_code()}</span>
		{#if !streaming}
			<CopyButton
				value={code}
				scopeKey={`code:${lang}`}
				errorMessage={m.agent_copy_failed()}
				size="xs"
				variant="ghost"
			/>
		{/if}
	</div>
	<pre class="max-h-[28rem] overflow-auto p-3 text-xs leading-5"><code
			>{#if isDiff}{#each lines as line, index (index)}<span
						class={['block min-w-fit px-1', lineClass(line)]}>{line === '' ? ' ' : line}</span
					>{/each}{:else}{code}{/if}</code
		></pre>
</div>
