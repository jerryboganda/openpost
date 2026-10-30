<script lang="ts">
	import { onDestroy, onMount, tick } from 'svelte';
	import { AttachmentTray } from '$lib/agent/attachments.svelte';
	import { formatDuration } from '$lib/agent/format';
	import { AgentSession } from '$lib/agent/session.svelte';
	import { runDurationMs } from '$lib/agent/transcript';
	import type { AgentAttachment, AgentConversationSummary } from '$lib/agent/types';
	import AgentComposer from '$lib/components/agent/agent-composer.svelte';
	import AgentEmpty from '$lib/components/agent/agent-empty.svelte';
	import AgentImageViewer from '$lib/components/agent/agent-image-viewer.svelte';
	import AgentSidebar from '$lib/components/agent/agent-sidebar.svelte';
	import AgentTranscript from '$lib/components/agent/agent-transcript.svelte';
	import DestructiveConfirmDialog from '$lib/components/destructive-confirm-dialog.svelte';
	import InlineNotice from '$lib/components/inline-notice.svelte';
	import PageContainer from '$lib/components/page-container.svelte';
	import { Button } from '$lib/components/ui/button';
	import { getLocaleTag } from '$lib/i18n';
	import { m } from '$lib/paraglide/messages';
	import { auth } from '$lib/stores/auth';
	import { ThemeIcon } from '$lib/themes/icons';
	import { cn } from '$lib/utils';

	const session = new AgentSession();
	const tray = new AttachmentTray();
	const locale = getLocaleTag();

	let draft = $state('');
	let textarea = $state<HTMLTextAreaElement | null>(null);
	let listOpen = $state(false);
	let viewing = $state<AgentAttachment | null>(null);
	let dragging = $state(false);
	let deleteOpen = $state(false);
	let deleting = $state<AgentConversationSummary | null>(null);
	let started = false;

	const isAdmin = $derived(Boolean($auth.user?.is_admin));
	const title = $derived(session.active?.title.trim() || m.agent_new_chat());
	const elapsed = $derived(
		session.activeRun ? formatDuration(runDurationMs(session.activeRun, session.now)) : ''
	);
	const unavailableReason = $derived.by(() => {
		const id = session.active?.engine ?? session.engine;
		const engine = session.engines.find((candidate) => candidate.id === id);
		return engine && !engine.available ? (engine.reason ?? m.agent_engine_unavailable()) : '';
	});

	$effect(() => {
		if (isAdmin && !started) {
			started = true;
			void session.start();
		} else if (!isAdmin) {
			session.loading = false;
		}
	});

	onMount(() => {
		const refocus = () => {
			if (document.visibilityState === 'visible' && session.activeRun && session.connection !== 'live') {
				void session.resume();
			}
		};
		document.addEventListener('visibilitychange', refocus);
		return () => document.removeEventListener('visibilitychange', refocus);
	});

	onDestroy(() => {
		session.dispose();
		tray.clear();
	});

	async function focusComposer() {
		await tick();
		textarea?.focus();
	}

	async function submit() {
		const text = draft;
		const images = tray.ready.flatMap((item) => (item.attachment ? [item.attachment] : []));
		draft = '';
		const sent = await session.send(text, images);
		if (sent) {
			tray.clear();
		} else if (draft === '') {
			draft = text;
		}
	}

	function newChat() {
		session.startNew();
		tray.clear();
		draft = '';
		listOpen = false;
		void focusComposer();
	}

	async function select(id: string) {
		listOpen = false;
		if (id !== session.active?.id) await session.open(id);
		void focusComposer();
	}

	function askToDelete(conversation: AgentConversationSummary) {
		deleting = conversation;
		deleteOpen = true;
	}

	function pick(prompt: string) {
		draft = prompt;
		void focusComposer().then(() => textarea?.setSelectionRange(prompt.length, prompt.length));
	}

	function carriesFiles(event: DragEvent): boolean {
		return (
			unavailableReason === '' &&
			session.builtIn &&
			(event.dataTransfer?.types.includes('Files') ?? false)
		);
	}

	function onDragOver(event: DragEvent) {
		if (!carriesFiles(event)) return;
		event.preventDefault();
		dragging = true;
	}

	function onDragLeave(event: DragEvent) {
		const inside =
			event.currentTarget instanceof Node &&
			event.relatedTarget instanceof Node &&
			event.currentTarget.contains(event.relatedTarget);
		if (!inside) dragging = false;
	}

	function onDrop(event: DragEvent) {
		if (!carriesFiles(event)) return;
		event.preventDefault();
		dragging = false;
		tray.add(Array.from(event.dataTransfer?.files ?? []));
	}
</script>

<PageContainer
	title={m.agent_title()}
	themeIconRole="assistant"
	contentLayout="fill"
	headerActionLayout="inline"
	loading={isAdmin && session.loading}
	loadingLayout="list"
	loadingMessage={m.common_loading()}
>
	{#snippet actions()}
		{#if isAdmin}
			<Button variant="outline" onclick={newChat}>
				<ThemeIcon role="add" />
				{m.agent_new_chat()}
			</Button>
		{/if}
	{/snippet}

	{#if !isAdmin}
		<InlineNotice tone="error" message={m.agent_admin_only()} />
	{:else}
		<div class="flex min-h-0 flex-1 flex-col gap-3">
			{#if session.error}
				<InlineNotice
					tone="error"
					message={session.error}
					onDismiss={() => (session.error = '')}
					dismissLabel={m.common_dismiss()}
				/>
			{/if}
			<div
				class="min-h-[30rem] flex-1 overflow-hidden rounded-lg border bg-card lg:grid lg:grid-cols-[17rem_minmax(0,1fr)]"
			>
				<AgentSidebar
					class={cn('h-full', listOpen ? 'flex' : 'hidden lg:flex')}
					conversations={session.conversations}
					activeId={session.active?.id ?? ''}
					busy={session.busy}
					now={session.now}
					{locale}
					onSelect={(id) => void select(id)}
					onDelete={askToDelete}
				/>
				<!-- Dropping images is a shortcut. The attach button does the same by keyboard. -->
				<!-- svelte-ignore a11y_no_static_element_interactions -->
				<section
					class={cn(
						'relative h-full min-h-0 min-w-0 flex-col bg-background',
						listOpen ? 'hidden lg:flex' : 'flex'
					)}
					aria-label={title}
					ondragenter={onDragOver}
					ondragover={onDragOver}
					ondragleave={onDragLeave}
					ondrop={onDrop}
				>
					<header class="flex min-h-12 shrink-0 items-center gap-2 border-b px-3">
						<Button
							variant="ghost"
							size="icon-sm"
							class="lg:hidden"
							aria-label={m.agent_chats()}
							onclick={() => (listOpen = true)}
						>
							<ThemeIcon role="menu" />
						</Button>
						<h2 class="min-w-0 flex-1 truncate text-sm font-medium">{title}</h2>
						{#if session.connection === 'reconnecting'}
							<span class="shrink-0 text-xs text-muted-foreground" role="status">
								{m.agent_connection_reconnecting()}
							</span>
						{:else if session.connection === 'lost'}
							<span class="hidden shrink-0 text-xs text-muted-foreground sm:inline" role="status">
								{m.agent_connection_lost()}
							</span>
							<Button variant="outline" size="xs" onclick={() => void session.reconnect()}>
								{m.agent_connection_retry()}
							</Button>
						{:else if elapsed !== ''}
							<span class="shrink-0 text-xs text-muted-foreground tabular-nums">{elapsed}</span>
						{/if}
					</header>
					<AgentTranscript
						turns={session.turns}
						pending={session.pending}
						conversationId={session.active?.id ?? ''}
						now={session.now}
						{locale}
						models={session.models}
						canRetry={!session.working && unavailableReason === ''}
						onRetry={(text) => void session.send(text, [])}
						onOpenImage={(image) => (viewing = image)}
					>
						{#snippet empty()}
							<AgentEmpty onPick={pick} />
						{/snippet}
					</AgentTranscript>
					<AgentComposer
						{session}
						{tray}
						bind:value={draft}
						bind:textarea
						{unavailableReason}
						onSend={() => void submit()}
					/>
					{#if dragging}
						<div
							class="pointer-events-none absolute inset-2 z-10 flex items-center justify-center rounded-xl border-2 border-dashed border-ring bg-background/85 text-sm font-medium"
						>
							{m.agent_drop_images()}
						</div>
					{/if}
				</section>
			</div>
		</div>
	{/if}
</PageContainer>

<AgentImageViewer bind:image={viewing} />

<DestructiveConfirmDialog
	bind:open={deleteOpen}
	title={m.agent_delete_title()}
	description={m.agent_delete_description()}
	onConfirm={() => session.remove(deleting?.id ?? '')}
/>
