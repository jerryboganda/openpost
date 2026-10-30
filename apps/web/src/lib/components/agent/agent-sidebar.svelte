<script lang="ts">
	import { formatRelativeTime } from '$lib/agent/format';
	import { BUILT_IN_ENGINE, type AgentConversationSummary } from '$lib/agent/types';
	import { Button } from '$lib/components/ui/button';
	import { m } from '$lib/paraglide/messages';
	import { ProtectedIcon, ThemeIcon } from '$lib/themes/icons';
	import { cn } from '$lib/utils';

	interface Props {
		conversations: AgentConversationSummary[];
		activeId: string;
		/** Conversations with a task running. */
		busy: string[];
		now: number;
		locale: string;
		onSelect: (id: string) => void;
		onDelete: (conversation: AgentConversationSummary) => void;
		class?: string;
	}

	let {
		conversations,
		activeId,
		busy,
		now,
		locale,
		onSelect,
		onDelete,
		class: className
	}: Props = $props();

	function titleOf(conversation: AgentConversationSummary): string {
		return conversation.title.trim() || m.agent_new_chat();
	}
</script>

<nav class={cn('flex min-h-0 flex-col border-r', className)} aria-label={m.agent_chats()}>
	<h2 class="shrink-0 border-b px-4 py-3 text-sm font-medium">{m.agent_chats()}</h2>
	{#if conversations.length === 0}
		<p class="px-4 py-4 text-sm text-muted-foreground">{m.agent_chats_empty()}</p>
	{:else}
		<ul class="min-h-0 flex-1 space-y-0.5 overflow-y-auto overscroll-contain p-1.5">
			{#each conversations as conversation (conversation.id)}
				{@const active = conversation.id === activeId}
				<li class="group relative">
					<button
						type="button"
						class={cn(
							'flex w-full flex-col gap-0.5 rounded-lg px-3 py-2 pr-11 text-left hover:bg-muted/60 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none',
							active && 'bg-muted'
						)}
						aria-current={active ? 'true' : undefined}
						onclick={() => onSelect(conversation.id)}
					>
						<span class="flex min-w-0 items-center gap-2">
							{#if busy.includes(conversation.id)}
								<ProtectedIcon
									icon="loading"
									class="size-3.5 shrink-0 animate-spin motion-reduce:animate-none"
								/>
							{/if}
							<span class="truncate text-sm font-medium">{titleOf(conversation)}</span>
						</span>
						<span class="truncate text-xs text-muted-foreground">
							{formatRelativeTime(conversation.updated_at, now, locale)}
							{#if conversation.engine !== BUILT_IN_ENGINE}
								· {m.agent_workflow()}
							{/if}
						</span>
					</button>
					<Button
						variant="ghost"
						size="icon-xs"
						class="absolute top-2 right-1.5 opacity-0 group-focus-within:opacity-100 group-hover:opacity-100 [@media(pointer:coarse)]:opacity-100"
						aria-label={m.agent_delete_chat_named({ title: titleOf(conversation) })}
						onclick={() => onDelete(conversation)}
					>
						<ThemeIcon role="delete" />
					</Button>
				</li>
			{/each}
		</ul>
	{/if}
</nav>
