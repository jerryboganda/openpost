<script lang="ts">
	import { formatCount, formatDuration } from '$lib/agent/format';
	import { runDurationMs, type Turn } from '$lib/agent/transcript';
	import type { AgentAttachment, AgentModel } from '$lib/agent/types';
	import CopyButton from '$lib/components/copy-button.svelte';
	import InlineNotice from '$lib/components/inline-notice.svelte';
	import { Button } from '$lib/components/ui/button';
	import { m } from '$lib/paraglide/messages';
	import { ProtectedIcon } from '$lib/themes/icons';
	import AgentMarkdown from './agent-markdown.svelte';
	import AgentToolCard from './agent-tool-card.svelte';
	import AgentUserMessage from './agent-user-message.svelte';

	interface Props {
		turn: Turn;
		now: number;
		locale: string;
		models: AgentModel[];
		/** The composer can take a new message, so a failed message can be sent again. */
		canRetry: boolean;
		onRetry: (text: string) => void;
		onOpenImage: (image: AgentAttachment) => void;
	}

	let { turn, now, locale, models, canRetry, onRetry, onOpenImage }: Props = $props();

	const run = $derived(turn.run);
	const elapsed = $derived(run ? formatDuration(runDurationMs(run, now)) : '');
	const modelLabel = $derived(
		models.find((option) => option.id === run?.model)?.label ?? run?.model ?? ''
	);
	const summary = $derived([modelLabel, elapsed].filter((part) => part !== '').join(' · '));
	const tokens = $derived(
		run && (run.input_tokens ?? 0) + (run.output_tokens ?? 0) > 0
			? m.agent_tokens({
					input: formatCount(run.input_tokens ?? 0, locale),
					output: formatCount(run.output_tokens ?? 0, locale)
				})
			: ''
	);
	const reply = $derived(
		turn.blocks.flatMap((block) => (block.kind === 'text' ? [block.text] : [])).join('\n\n')
	);
	const retryText = $derived(
		turn.user && (turn.user.attachments ?? []).length === 0 ? turn.user.content : ''
	);
</script>

<article class="flex flex-col gap-4" data-slot="agent-turn" data-status={run?.status}>
	{#if turn.user}
		<AgentUserMessage
			text={turn.user.content}
			images={turn.user.attachments ?? []}
			{onOpenImage}
		/>
	{/if}
	<div class="flex min-w-0 flex-col gap-3">
		{#each turn.blocks as block (block.id)}
			{#if block.kind === 'text'}
				<AgentMarkdown source={block.text} />
			{:else if block.kind === 'tool'}
				<AgentToolCard step={block} />
			{:else if block.tone === 'error'}
				<InlineNotice tone="error" size="compact" message={block.text} />
			{:else}
				<p class="flex items-start gap-2 text-xs text-muted-foreground">
					<ProtectedIcon icon="info" class="mt-0.5 size-3.5 shrink-0" />
					<span class="min-w-0 break-words">{block.text}</span>
				</p>
			{/if}
		{/each}
		{#if turn.active}
			{#if turn.live !== ''}
				<div aria-live="off" aria-busy="true">
					<AgentMarkdown source={turn.live} />
				</div>
			{/if}
			<p class="flex items-center gap-2 text-xs text-muted-foreground" role="status">
				<ProtectedIcon icon="loading" class="size-3.5 animate-spin motion-reduce:animate-none" />
				<span class="tabular-nums">{m.agent_working_for({ duration: elapsed })}</span>
			</p>
		{:else if run}
			<footer class="flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
				{#if summary !== ''}<span>{summary}</span>{/if}
				{#if tokens !== ''}<span class="tabular-nums">{tokens}</span>{/if}
				{#if run.workflow_url}
					<a
						href={run.workflow_url}
						target="_blank"
						rel="noopener noreferrer"
						class="underline underline-offset-2 hover:text-foreground"
						>{m.agent_view_workflow()}</a
					>
				{/if}
				{#if run.branch}
					<span class="min-w-0 break-words"
						>{m.agent_branch()}: <span class="font-mono">{run.branch}</span></span
					>
				{/if}
				{#if reply !== ''}
					<CopyButton
						value={reply}
						scopeKey={turn.id}
						errorMessage={m.agent_copy_failed()}
						label={m.agent_copy_reply()}
						size="xs"
						variant="ghost"
					/>
				{/if}
				{#if run.status === 'failed' && canRetry && retryText !== ''}
					<Button variant="ghost" size="xs" onclick={() => onRetry(retryText)}>
						{m.common_retry()}
					</Button>
				{/if}
			</footer>
		{/if}
	</div>
</article>
