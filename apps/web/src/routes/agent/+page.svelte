<script lang="ts">
	import { onDestroy, onMount } from 'svelte';
	import { client } from '$lib/api/client';
	import { auth } from '$lib/stores/auth';
	import PageContainer from '$lib/components/page-container.svelte';
	import EmptyState from '$lib/components/empty-state.svelte';
	import InlineNotice from '$lib/components/inline-notice.svelte';
	import PageLoading from '$lib/components/page-loading.svelte';
	import { Button } from '$lib/components/ui/button';
	import { Input } from '$lib/components/ui/input';
	import { Textarea } from '$lib/components/ui/textarea';
	import * as Select from '$lib/components/ui/select';
	import { getLocaleTag } from '$lib/i18n';
	import { m } from '$lib/paraglide/messages';

	type Engine = { id: string; label: string; available: boolean; reason?: string };
	type Conversation = { id: string; engine: string; title: string; updated_at: string };
	type Message = { id: string; role: string; content: string; created_at: string };
	type Run = {
		id: string;
		engine: string;
		status: string;
		summary?: string;
		failure_code?: string;
		failure_message?: string;
		branch?: string;
		workflow_url?: string;
		created_at: string;
	};
	type RunEvent = {
		id: string;
		run_id: string;
		seq: number;
		kind: string;
		message: string;
		created_at: string;
	};
	type ConversationView = Conversation & {
		messages: Message[];
		runs: Run[];
		events: RunEvent[];
	};

	const runActive = (status: string) =>
		status === 'queued' || status === 'dispatched' || status === 'running';

	let authState = $derived($auth);
	let isAdmin = $derived(Boolean(authState.user?.is_admin));
	let loading = $state(true);
	let error = $state('');
	let engines = $state<Engine[]>([]);
	let conversations = $state<Conversation[]>([]);
	let active = $state<ConversationView | null>(null);
	let prompt = $state('');
	let sending = $state(false);
	let creating = $state(false);
	let newEngine = $state('assistant');
	let newTitle = $state('');
	let stream: EventSource | null = null;
	let lastEventSeq = 0;

	const selectedEngine = $derived(engines.find((engine) => engine.id === newEngine) ?? null);
	const activeRun = $derived(active?.runs.find((run) => runActive(run.status)) ?? null);
	const latestRun = $derived(active?.runs.at(-1) ?? null);

	onMount(() => {
		void loadWorkspace();
	});

	onDestroy(() => closeStream());

	async function loadWorkspace() {
		if (!isAdmin) {
			loading = false;
			return;
		}
		loading = true;
		error = '';
		try {
			const [engineResponse, conversationResponse] = await Promise.all([
				client.GET('/admin/agent/engines'),
				client.GET('/admin/agent/conversations')
			]);
			if (engineResponse.error || conversationResponse.error) {
				error = m.agent_load_failed();
				return;
			}
			engines = engineResponse.data?.engines ?? [];
			conversations = conversationResponse.data?.conversations ?? [];
			if (engines.length > 0) {
				const current = engines.find((engine) => engine.id === newEngine);
				if (!current || !current.available) {
					newEngine = engines.find((engine) => engine.available)?.id ?? engines[0].id;
				}
			}
			if (conversations.length > 0) {
				await openConversation(conversations[0].id);
			}
		} catch {
			error = m.agent_load_failed();
		} finally {
			loading = false;
		}
	}

	async function openConversation(conversationID: string) {
		closeStream();
		const response = await client.GET('/admin/agent/conversations/{conversation_id}', {
			params: { path: { conversation_id: conversationID } }
		});
		if (response.error || !response.data) {
			error = m.agent_load_failed();
			return;
		}
		active = response.data.conversation;
		error = '';
		lastEventSeq = active.events.reduce((max, event) => Math.max(max, event.seq), 0);
		const running = active.runs.find((run) => runActive(run.status));
		if (running) startStream(running.id, lastEventSeq);
	}

	function startStream(runID: string, after: number) {
		closeStream();
		const source = new EventSource(`/api/v1/admin/agent/runs/${runID}/events?after=${after}`);
		source.addEventListener('run', (event) => {
			// SAFETY: EventSource dispatches a MessageEvent for named events, and the server serializes the Run model as JSON.
			const run = JSON.parse((event as MessageEvent).data) as Run;
			upsertRun(run);
		});
		source.addEventListener('event', (event) => {
			// SAFETY: EventSource dispatches a MessageEvent for named events, and the server serializes Event records as JSON.
			const update = JSON.parse((event as MessageEvent).data) as RunEvent;
			if (active && !active.events.some((existing) => existing.id === update.id)) {
				active.events.push(update);
				lastEventSeq = Math.max(lastEventSeq, update.seq);
			}
		});
		source.addEventListener('done', () => {
			closeStream();
			if (active) void openConversation(active.id);
		});
		source.addEventListener('stream_error', () => {
			error = m.agent_load_failed();
			closeStream();
		});
		source.onerror = () => closeStream();
		stream = source;
	}

	function closeStream() {
		stream?.close();
		stream = null;
	}

	function upsertRun(run: Run) {
		if (!active) return;
		const index = active.runs.findIndex((existing) => existing.id === run.id);
		if (index >= 0) active.runs[index] = run;
		else active.runs.push(run);
	}

	async function createConversation() {
		if (creating) return;
		creating = true;
		error = '';
		try {
			const response = await client.POST('/admin/agent/conversations', {
				body: { engine: newEngine, title: newTitle.trim() }
			});
			if (response.error || !response.data) {
				error = m.agent_unknown_error();
				return;
			}
			newTitle = '';
			const conversation = response.data.conversation;
			conversations = [conversation, ...conversations];
			await openConversation(conversation.id);
		} catch {
			error = m.agent_unknown_error();
		} finally {
			creating = false;
		}
	}

	async function sendTask() {
		if (!active || !prompt.trim() || sending) return;
		sending = true;
		error = '';
		try {
			const response = await client.POST('/admin/agent/conversations/{conversation_id}/messages', {
				params: { path: { conversation_id: active.id } },
				body: { prompt: prompt.trim() }
			});
			if (response.error || !response.data) {
				error = m.agent_unknown_error();
				return;
			}
			prompt = '';
			const run = response.data.run;
			if (run.status === 'failed') {
				error = run.failure_message || m.agent_unknown_error();
			}
			await openConversation(active.id);
		} catch {
			error = m.agent_unknown_error();
		} finally {
			sending = false;
		}
	}

	async function cancelRun(runID: string) {
		const response = await client.POST('/admin/agent/runs/{run_id}/cancel', {
			params: { path: { run_id: runID } }
		});
		if (response.error) {
			error = m.agent_unknown_error();
			return;
		}
		if (active) await openConversation(active.id);
	}

	function statusLabel(status: string) {
		switch (status) {
			case 'queued':
				return m.agent_status_queued();
			case 'dispatched':
				return m.agent_status_dispatched();
			case 'running':
				return m.agent_status_running();
			case 'succeeded':
				return m.agent_status_succeeded();
			case 'failed':
				return m.agent_status_failed();
			case 'canceled':
				return m.agent_status_canceled();
			default:
				return status;
		}
	}

	function statusTone(status: string) {
		if (status === 'succeeded') return 'text-success-foreground';
		if (status === 'failed') return 'text-action-destructive-foreground';
		if (status === 'canceled') return 'text-muted-foreground';
		return 'text-info-foreground';
	}

	function formatTime(value: string) {
		return new Intl.DateTimeFormat(getLocaleTag(), {
			dateStyle: 'medium',
			timeStyle: 'short'
		}).format(new Date(value));
	}
</script>

<PageContainer title={m.agent_title()} description={m.agent_description()} themeIconRole="settings">
	{#if !isAdmin}
		<InlineNotice tone="error" message={m.agent_admin_only()} />
	{:else if loading}
		<PageLoading layout="list" label={m.common_loading()} items={4} />
	{:else}
		<div class="grid gap-6 lg:grid-cols-[18rem_minmax(0,1fr)]">
			<aside class="space-y-4">
				<div class="rounded-lg border bg-muted/20 p-4">
					<h2 class="text-sm font-semibold">{m.agent_new_conversation()}</h2>
					<div class="mt-3 space-y-3">
						<label class="block text-sm">
							<span class="text-muted-foreground">{m.agent_engine()}</span>
							<Select.Root
								type="single"
								value={newEngine}
								onValueChange={(value) => (newEngine = value)}
							>
								<Select.Trigger class="mt-1 w-full" aria-label={m.agent_engine()}>
									{selectedEngine?.label ?? newEngine}
								</Select.Trigger>
								<Select.Content>
									{#each engines as engine (engine.id)}
										<Select.Item value={engine.id} disabled={!engine.available}>
											{engine.label}{engine.available ? '' : ` — ${engine.reason ?? ''}`}
										</Select.Item>
									{/each}
								</Select.Content>
							</Select.Root>
						</label>
						{#if selectedEngine && !selectedEngine.available}
							<InlineNotice
								tone="warning"
								message={selectedEngine.reason ?? m.agent_engine_unavailable()}
							/>
						{/if}
						<label class="block text-sm">
							<span class="text-muted-foreground">{m.agent_title_label()}</span>
							<Input class="mt-1" bind:value={newTitle} placeholder={m.agent_title_placeholder()} />
						</label>
						<Button
							size="sm"
							class="w-full"
							disabled={creating || !newEngine || !selectedEngine?.available}
							onclick={createConversation}
						>
							{creating ? m.agent_sending() : m.agent_create()}
						</Button>
					</div>
				</div>

				<div>
					<h2 class="px-1 text-sm font-semibold">{m.agent_conversations()}</h2>
					<ul class="mt-2 space-y-1">
						{#each conversations as conversation (conversation.id)}
							<li>
								<button
									type="button"
									class="w-full rounded-md px-3 py-2 text-left text-sm hover:bg-muted"
									class:bg-muted={active?.id === conversation.id}
									onclick={() => openConversation(conversation.id)}
								>
									<span class="block truncate font-medium">
										{conversation.title || conversation.engine}
									</span>
									<span class="text-xs text-muted-foreground">
										{conversation.engine} · {formatTime(conversation.updated_at)}
									</span>
								</button>
							</li>
						{/each}
					</ul>
				</div>
			</aside>

			<section class="min-w-0 space-y-4">
				{#if error}
					<InlineNotice tone="error" message={error} />
				{/if}

				{#if !active}
					<EmptyState title={m.agent_title()} description={m.agent_empty()} />
				{:else}
					<div class="space-y-3">
						{#each active.messages as message (message.id)}
							<article class="rounded-lg border p-3" class:bg-muted={message.role === 'user'}>
								<p class="text-xs font-medium text-muted-foreground">
									{message.role === 'user' ? m.agent_you() : m.agent_agent()}
								</p>
								<p class="mt-1 text-sm whitespace-pre-wrap">{message.content}</p>
							</article>
						{/each}
					</div>

					{#if latestRun}
						<div class="rounded-lg border p-3" data-testid="agent-run">
							<div class="flex flex-wrap items-center justify-between gap-2">
								<p class="text-sm font-medium">
									{m.agent_run_status()}:
									<span class={statusTone(latestRun.status)}>
										{statusLabel(latestRun.status)}
									</span>
								</p>
								<div class="flex items-center gap-2">
									{#if latestRun.workflow_url}
										<Button
											href={latestRun.workflow_url}
											target="_blank"
											rel="noreferrer"
											variant="outline"
											size="sm"
										>
											{m.agent_workflow()}
										</Button>
									{/if}
									{#if runActive(latestRun.status)}
										<Button variant="outline" size="sm" onclick={() => cancelRun(latestRun.id)}>
											{m.agent_cancel_run()}
										</Button>
									{/if}
								</div>
							</div>
							{#if latestRun.summary}
								<p class="mt-2 text-sm whitespace-pre-wrap">{latestRun.summary}</p>
							{/if}
							{#if latestRun.failure_message}
								<p class="mt-2 text-sm text-action-destructive-foreground">
									{latestRun.failure_message}
								</p>
							{/if}
							{#if latestRun.branch}
								<p class="mt-2 text-xs text-muted-foreground">
									{m.agent_branch()}: <span class="font-mono">{latestRun.branch}</span>
								</p>
							{/if}
						</div>
					{/if}

					<div>
						<h2 class="text-sm font-semibold">{m.agent_run_events()}</h2>
						{#if active.events.length === 0}
							<p class="mt-1 text-sm text-muted-foreground">{m.agent_no_events()}</p>
						{:else}
							<ol class="mt-2 space-y-2">
								{#each active.events as event (event.id)}
									<li class="flex gap-3 text-sm">
										<span class="w-32 shrink-0 text-xs text-muted-foreground">
											{formatTime(event.created_at)}
										</span>
										<span class="min-w-0">
											<span class="font-medium">{event.kind}</span>
											<span class="text-muted-foreground"> · {event.message}</span>
										</span>
									</li>
								{/each}
							</ol>
						{/if}
					</div>

					<form
						class="space-y-2"
						onsubmit={(event) => {
							event.preventDefault();
							void sendTask();
						}}
					>
						<Textarea
							bind:value={prompt}
							placeholder={m.agent_prompt_placeholder()}
							rows={4}
							disabled={sending || Boolean(activeRun)}
						/>
						<div class="flex items-center justify-between gap-2">
							<p class="text-xs text-muted-foreground">{m.agent_description()}</p>
							<Button
								type="submit"
								size="sm"
								disabled={sending || !prompt.trim() || Boolean(activeRun)}
							>
								{sending ? m.agent_sending() : m.agent_send()}
							</Button>
						</div>
					</form>
				{/if}
			</section>
		</div>
	{/if}
</PageContainer>
