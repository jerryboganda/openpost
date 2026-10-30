<script lang="ts">
	import { tick, type Snippet } from 'svelte';
	import type { Turn } from '$lib/agent/transcript';
	import type { PendingMessage } from '$lib/agent/session.svelte';
	import type { AgentAttachment, AgentModel } from '$lib/agent/types';
	import { Button } from '$lib/components/ui/button';
	import { m } from '$lib/paraglide/messages';
	import { ProtectedIcon, ThemeIcon } from '$lib/themes/icons';
	import AgentTurn from './agent-turn.svelte';
	import AgentUserMessage from './agent-user-message.svelte';

	interface Props {
		turns: Turn[];
		pending: PendingMessage | null;
		/** Changes when another conversation opens, so the view starts at the bottom. */
		conversationId: string;
		now: number;
		locale: string;
		models: AgentModel[];
		canRetry: boolean;
		onRetry: (text: string) => void;
		onOpenImage: (image: AgentAttachment) => void;
		empty: Snippet;
	}

	const NEAR_BOTTOM = 96;

	let {
		turns,
		pending,
		conversationId,
		now,
		locale,
		models,
		canRetry,
		onRetry,
		onOpenImage,
		empty
	}: Props = $props();

	let scroller = $state<HTMLElement | null>(null);
	let content = $state<HTMLElement | null>(null);
	let atBottom = $state(true);
	let following = true;

	function prefersReducedMotion(): boolean {
		return globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches ?? false;
	}

	function jump(smooth: boolean) {
		scroller?.scrollTo({
			top: scroller.scrollHeight,
			behavior: smooth && !prefersReducedMotion() ? 'smooth' : 'auto'
		});
	}

	function track() {
		if (!scroller) return;
		atBottom = scroller.scrollHeight - scroller.scrollTop - scroller.clientHeight < NEAR_BOTTOM;
		following = atBottom;
	}

	// Streaming text, images, and opened steps all grow the content. The view
	// follows that growth only while the reader is at the bottom.
	$effect(() => {
		if (!content) return;
		const observer = new ResizeObserver(() => {
			if (following) jump(false);
		});
		observer.observe(content);
		return () => observer.disconnect();
	});

	$effect(() => {
		void conversationId;
		following = true;
		void tick().then(() => jump(false));
	});

	$effect(() => {
		if (!pending) return;
		following = true;
		void tick().then(() => jump(true));
	});
</script>

<div class="relative min-h-0 flex-1">
	<div
		bind:this={scroller}
		class="h-full overflow-y-auto overscroll-contain"
		role="log"
		aria-label={m.agent_transcript()}
		onscroll={track}
	>
		<div bind:this={content} class="mx-auto flex w-full max-w-3xl flex-col gap-8 px-4 py-6">
			{#if turns.length === 0 && !pending}
				{@render empty()}
			{:else}
				{#each turns as turn (turn.id)}
					<AgentTurn {turn} {now} {locale} {models} {canRetry} {onRetry} {onOpenImage} />
				{/each}
				{#if pending}
					<div class="flex flex-col gap-4" data-slot="agent-pending">
						<AgentUserMessage text={pending.text} images={pending.images} {onOpenImage} />
						<p class="flex items-center gap-2 text-xs text-muted-foreground" role="status">
							<ProtectedIcon
								icon="loading"
								class="size-3.5 animate-spin motion-reduce:animate-none"
							/>
							{m.agent_working()}
						</p>
					</div>
				{/if}
			{/if}
		</div>
	</div>
	{#if !atBottom}
		<Button
			variant="outline"
			size="sm"
			class="absolute bottom-3 left-1/2 -translate-x-1/2 rounded-full shadow-md"
			onclick={() => {
				following = true;
				jump(true);
			}}
		>
			<ThemeIcon role="arrow-down" />
			{m.agent_jump_to_latest()}
		</Button>
	{/if}
</div>
