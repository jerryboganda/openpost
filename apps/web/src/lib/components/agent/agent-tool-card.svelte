<script lang="ts">
	import { formatDuration } from '$lib/agent/format';
	import { toolIcon } from '$lib/agent/tool-icons';
	import type { ToolStep } from '$lib/agent/transcript';
	import { m } from '$lib/paraglide/messages';
	import { ProtectedIcon, ThemeIcon } from '$lib/themes/icons';
	import { cn } from '$lib/utils';
	import AgentCodeBlock from './agent-code-block.svelte';
	import AgentDiff from './agent-diff.svelte';

	interface Props {
		step: ToolStep;
	}

	const WRITE_PREVIEW_LINES = 80;

	let { step }: Props = $props();

	const panelId = $props.id();
	let open = $state(false);
	let toggled = false;

	// A failure opens by itself, so its reason is visible without a click.
	$effect(() => {
		if (step.state === 'error' && !toggled) open = true;
	});

	const duration = $derived(step.durationMs === null ? '' : formatDuration(step.durationMs));
	const preview = $derived(
		(step.args.content ?? '').split('\n').slice(0, WRITE_PREVIEW_LINES).join('\n')
	);
	const edit = $derived(
		step.name === 'repo_edit' &&
			step.args.old_string !== undefined &&
			step.args.new_string !== undefined
			? { before: step.args.old_string, after: step.args.new_string }
			: null
	);

	function toggle() {
		toggled = true;
		open = !open;
	}
</script>

<div class="rounded-lg border bg-background/40" data-slot="agent-tool" data-state={step.state}>
	<button
		type="button"
		class="flex min-h-9 w-full items-center gap-2 rounded-lg px-2.5 py-1.5 text-left text-sm hover:bg-muted/50 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none focus-visible:ring-inset [@media(pointer:coarse)]:min-h-11"
		aria-expanded={open}
		aria-controls={panelId}
		onclick={toggle}
	>
		<ThemeIcon role={toolIcon(step.name)} class="size-4 shrink-0 text-muted-foreground" />
		<span class="min-w-0 flex-1 truncate">{step.label}</span>
		{#if step.state === 'running'}
			<span class="flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground">
				<ProtectedIcon icon="loading" class="size-3.5 animate-spin motion-reduce:animate-none" />
				{m.agent_tool_running()}
			</span>
		{:else if step.state === 'error'}
			<span
				class="flex shrink-0 items-center gap-1.5 text-xs text-action-destructive-foreground"
			>
				<ProtectedIcon icon="error" class="size-3.5" />
				{m.agent_tool_failed()}
			</span>
		{:else if step.state === 'stopped'}
			<span class="shrink-0 text-xs text-muted-foreground">{m.agent_tool_stopped()}</span>
		{:else if duration}
			<span class="shrink-0 text-xs text-muted-foreground tabular-nums">{duration}</span>
		{/if}
		<ThemeIcon
			role="chevron-right"
			class={cn(
				'size-4 shrink-0 text-muted-foreground transition-transform motion-reduce:transition-none',
				open && 'rotate-90'
			)}
		/>
	</button>
	{#if open}
		<div id={panelId} class="flex flex-col gap-3 border-t px-3 py-3">
			{#if edit}
				<p class="min-w-0 text-xs break-words text-muted-foreground">
					<span class="font-mono">{step.args.path}</span>
					{#if step.args.branch}{m.agent_on_branch({ branch: step.args.branch })}{/if}
				</p>
				<AgentDiff before={edit.before} after={edit.after} />
			{:else if step.name === 'repo_write' && step.args.content}
				<p class="min-w-0 text-xs break-words text-muted-foreground">
					<span class="font-mono">{step.args.path}</span>
					{#if step.args.branch}{m.agent_on_branch({ branch: step.args.branch })}{/if}
				</p>
				<AgentCodeBlock code={preview} />
			{:else if step.name === 'db_query' && step.args.sql}
				<AgentCodeBlock code={step.args.sql} lang="sql" />
			{:else if step.input}
				<div>
					<p class="mb-1 text-xs font-medium text-muted-foreground">{m.agent_tool_input()}</p>
					<pre
						class="max-h-48 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs leading-5">{step.input}</pre>
				</div>
			{/if}
			{#if step.output}
				<div>
					<p class="mb-1 text-xs font-medium text-muted-foreground">{m.agent_tool_output()}</p>
					<pre
						class="max-h-72 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs leading-5">{step.output}</pre>
				</div>
			{:else if step.state !== 'running'}
				<p class="text-xs text-muted-foreground">{m.agent_tool_no_output()}</p>
			{/if}
		</div>
	{/if}
</div>
