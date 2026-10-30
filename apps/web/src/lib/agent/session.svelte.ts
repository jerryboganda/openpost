import { client } from '$lib/api/client';
import type { components } from '$lib/api/types';
import type { DestructiveActionOutcome } from '$lib/destructive-action-outcome';
import { m } from '$lib/paraglide/messages';
import {
	normalizePreferences,
	readLastConversation,
	readPreferences,
	storeLastConversation,
	storePreferences,
	type AgentPreferences
} from './preferences';
import { buildTranscript } from './transcript';
import {
	BUILT_IN_ENGINE,
	isRunActive,
	normalizeConversation,
	type AgentAccess,
	type AgentAttachment,
	type AgentConversation,
	type AgentConversationSummary,
	type AgentEffort,
	type AgentEngine,
	type AgentEvent,
	type AgentModel,
	type AgentRun
} from './types';

type ProblemDetails = components['schemas']['ErrorModel'];

const MAX_RECONNECTS = 5;

type Connection = 'idle' | 'live' | 'reconnecting' | 'lost';

/** A message that was sent and is not in the stored conversation yet. */
export interface PendingMessage {
	text: string;
	images: AgentAttachment[];
}

function problemText(error: ProblemDetails | undefined): string {
	return error?.detail?.trim() || m.agent_unknown_error();
}

function parseStreamRun(raw: string): AgentRun | null {
	try {
		const value = JSON.parse(raw);
		return typeof value?.id === 'string' && typeof value?.status === 'string' ? value : null;
	} catch {
		return null;
	}
}

function parseStreamEvent(raw: string): AgentEvent | null {
	try {
		const value = JSON.parse(raw);
		const valid =
			typeof value?.id === 'string' &&
			typeof value?.run_id === 'string' &&
			typeof value?.seq === 'number' &&
			typeof value?.kind === 'string' &&
			typeof value?.message === 'string';
		return valid ? value : null;
	} catch {
		return null;
	}
}

function parseLiveText(raw: string): string | null {
	try {
		const value = JSON.parse(raw);
		return typeof value?.text === 'string' ? value.text : null;
	} catch {
		return null;
	}
}

function lastSequence(conversation: AgentConversation, runId: string): number {
	return conversation.events
		.filter((event) => event.run_id === runId)
		.reduce((highest, event) => Math.max(highest, event.seq), 0);
}

function summarize(conversation: AgentConversation): AgentConversationSummary {
	return {
		id: conversation.id,
		engine: conversation.engine,
		title: conversation.title,
		workspace_id: conversation.workspace_id,
		created_at: conversation.created_at,
		updated_at: conversation.updated_at
	};
}

/**
 * State and actions of the agent page. The server owns every run, so closing
 * the tab never loses work: the page reads the stored conversation and, for a
 * run that is still going, follows its event stream from the last event seen.
 */
export class AgentSession {
	loading = $state(true);
	error = $state('');
	engines = $state<AgentEngine[]>([]);
	models = $state<AgentModel[]>([]);
	defaultModel = $state('');
	conversations = $state<AgentConversationSummary[]>([]);
	active = $state<AgentConversation | null>(null);
	pending = $state<PendingMessage | null>(null);
	liveText = $state('');
	sending = $state(false);
	connection = $state<Connection>('idle');
	now = $state(Date.now());
	engine = $state<string>(BUILT_IN_ENGINE);
	preferences = $state<AgentPreferences>({ model: '', effort: 'auto', access: 'full' });
	/** Conversations that have a task running, as far as this page has seen. */
	busy = $state<string[]>([]);

	turns = $derived(buildTranscript(this.active, this.liveText));
	activeRun = $derived(this.active?.runs.find((run) => isRunActive(run.status)) ?? null);
	working = $derived(this.sending || this.activeRun !== null);
	builtIn = $derived((this.active?.engine ?? this.engine) === BUILT_IN_ENGINE);
	selectedModel = $derived(
		this.models.find((option) => option.id === this.preferences.model) ?? null
	);

	#stream: EventSource | null = null;
	#retry: ReturnType<typeof setTimeout> | undefined;
	#clock: ReturnType<typeof setInterval> | undefined;
	#attempts = 0;
	#opening = 0;
	#disposed = false;

	async start() {
		this.loading = true;
		try {
			const engines = await client.GET('/admin/agent/engines');
			if (engines.error || !engines.data) {
				this.error = m.agent_load_failed();
				return;
			}
			this.engines = engines.data.engines ?? [];
			this.models = engines.data.models ?? [];
			this.defaultModel = engines.data.default_model;
			this.preferences = normalizePreferences(readPreferences(), this.models, this.defaultModel);
			const available = this.engines.filter((candidate) => candidate.available);
			this.engine =
				available.find((candidate) => candidate.id === BUILT_IN_ENGINE)?.id ??
				available[0]?.id ??
				BUILT_IN_ENGINE;
			if (!(await this.refreshConversations())) {
				this.error = m.agent_load_failed();
				return;
			}
			const last = readLastConversation();
			if (last && this.conversations.some((conversation) => conversation.id === last)) {
				await this.open(last);
			}
		} catch {
			this.error = m.agent_load_failed();
		} finally {
			this.loading = false;
		}
	}

	async refreshConversations(): Promise<boolean> {
		const response = await client.GET('/admin/agent/conversations');
		if (response.error || !response.data) return false;
		this.conversations = response.data.conversations ?? [];
		this.now = Date.now();
		return true;
	}

	async open(id: string, announce = true): Promise<boolean> {
		this.#opening += 1;
		const ticket = this.#opening;
		this.#closeStream();
		const response = await client.GET('/admin/agent/conversations/{conversation_id}', {
			params: { path: { conversation_id: id } }
		});
		if (ticket !== this.#opening || this.#disposed) return false;
		if (response.error || !response.data) {
			if (announce) this.error = m.agent_load_failed();
			return false;
		}
		const conversation = normalizeConversation(response.data.conversation);
		this.error = '';
		this.active = conversation;
		this.pending = null;
		this.liveText = '';
		storeLastConversation(conversation.id);
		const run = conversation.runs.find((candidate) => isRunActive(candidate.status));
		this.#markBusy(conversation.id, run !== undefined);
		if (run) this.#follow(run.id, lastSequence(conversation, run.id));
		else this.connection = 'idle';
		this.#syncClock();
		return true;
	}

	startNew() {
		this.#opening += 1;
		this.#closeStream();
		this.active = null;
		this.pending = null;
		this.liveText = '';
		this.error = '';
		this.connection = 'idle';
		storeLastConversation('');
		this.#syncClock();
	}

	/** Sends a message. Returns false when nothing was sent, so the caller can restore its draft. */
	async send(prompt: string, images: AgentAttachment[]): Promise<boolean> {
		const text = prompt.trim();
		if (this.working || (text === '' && images.length === 0)) return false;
		this.sending = true;
		this.error = '';
		this.pending = { text, images };
		try {
			let conversationId = this.active?.id ?? '';
			if (!conversationId) {
				const created = await client.POST('/admin/agent/conversations', {
					body: { engine: this.engine }
				});
				if (created.error || !created.data) return this.#fail(problemText(created.error));
				const conversation = normalizeConversation(created.data.conversation);
				conversationId = conversation.id;
				this.active = conversation;
				this.conversations = [summarize(conversation), ...this.conversations];
				storeLastConversation(conversationId);
			}
			const started = await client.POST('/admin/agent/conversations/{conversation_id}/messages', {
				params: { path: { conversation_id: conversationId } },
				body: {
					prompt: text,
					attachment_ids: images.map((image) => image.id),
					model: this.preferences.model,
					effort: this.preferences.effort,
					access: this.preferences.access
				}
			});
			if (started.error || !started.data) return this.#fail(problemText(started.error));
			await this.open(conversationId);
			this.pending = null;
			void this.refreshConversations();
			return true;
		} catch {
			return this.#fail(m.agent_unknown_error());
		} finally {
			this.sending = false;
		}
	}

	async stop() {
		const run = this.activeRun;
		const conversation = this.active;
		if (!run || !conversation) return;
		const response = await client.POST('/admin/agent/runs/{run_id}/cancel', {
			params: { path: { run_id: run.id } }
		});
		if (response.error) this.error = problemText(response.error);
		await this.open(conversation.id);
		void this.refreshConversations();
	}

	async remove(id: string): Promise<DestructiveActionOutcome> {
		const response = await client.DELETE('/admin/agent/conversations/{conversation_id}', {
			params: { path: { conversation_id: id } }
		});
		if (response.error) return { ok: false, message: problemText(response.error) };
		this.conversations = this.conversations.filter((conversation) => conversation.id !== id);
		this.#markBusy(id, false);
		if (this.active?.id === id) this.startNew();
		return { ok: true };
	}

	/** Reads the stored conversation again and follows a run that is still going. */
	async resume() {
		const conversation = this.active;
		if (!conversation || this.#disposed) return;
		const runId = this.activeRun?.id ?? '';
		const loaded = await this.open(conversation.id, false);
		if (!loaded && runId) this.#retryLater(runId);
	}

	/** The administrator asked to try again after the connection was lost. */
	async reconnect() {
		this.#attempts = 0;
		await this.resume();
	}

	setModel(id: string) {
		this.preferences.model = id;
		storePreferences({ ...this.preferences });
	}

	setEffort(effort: AgentEffort) {
		this.preferences.effort = effort;
		storePreferences({ ...this.preferences });
	}

	setAccess(access: AgentAccess) {
		this.preferences.access = access;
		storePreferences({ ...this.preferences });
	}

	setEngine(id: string) {
		this.engine = id;
	}

	dispose() {
		this.#disposed = true;
		this.#closeStream();
		clearTimeout(this.#retry);
		clearInterval(this.#clock);
		this.#clock = undefined;
	}

	#fail(message: string): false {
		this.error = message;
		this.pending = null;
		return false;
	}

	#markBusy(id: string, busy: boolean) {
		const listed = this.busy.includes(id);
		if (busy && !listed) this.busy.push(id);
		if (!busy && listed) this.busy = this.busy.filter((candidate) => candidate !== id);
	}

	#follow(runId: string, after: number) {
		this.#closeStream();
		const source = new EventSource(
			`/api/v1/admin/agent/runs/${encodeURIComponent(runId)}/events?after=${after}`
		);
		this.#stream = source;
		this.connection = 'live';
		source.addEventListener('open', () => {
			this.#attempts = 0;
			this.connection = 'live';
		});
		source.addEventListener('run', (event: MessageEvent) => {
			const run = parseStreamRun(event.data);
			if (run) this.#upsertRun(run);
		});
		source.addEventListener('event', (event: MessageEvent) => {
			const update = parseStreamEvent(event.data);
			if (update) this.#appendEvent(update);
		});
		source.addEventListener('live', (event: MessageEvent) => {
			const text = parseLiveText(event.data);
			if (text !== null) this.liveText = text;
		});
		source.addEventListener('done', () => void this.#finish());
		source.addEventListener('stream_error', () => this.#dropped(runId));
		source.onerror = () => this.#dropped(runId);
	}

	#closeStream() {
		this.#stream?.close();
		this.#stream = null;
		clearTimeout(this.#retry);
	}

	#upsertRun(run: AgentRun) {
		const conversation = this.active;
		if (!conversation || run.conversation_id !== conversation.id) return;
		const index = conversation.runs.findIndex((existing) => existing.id === run.id);
		if (index >= 0) conversation.runs[index] = run;
		else conversation.runs.push(run);
		this.#markBusy(conversation.id, isRunActive(run.status));
		this.#syncClock();
	}

	#appendEvent(update: AgentEvent) {
		const conversation = this.active;
		if (!conversation || conversation.events.some((existing) => existing.id === update.id)) return;
		if (!conversation.runs.some((run) => run.id === update.run_id)) return;
		conversation.events.push(update);
		// The finished turn is now part of the transcript, so its live copy goes.
		if (update.kind === 'text') this.liveText = '';
	}

	async #finish() {
		this.#closeStream();
		this.connection = 'idle';
		const conversation = this.active;
		if (conversation) await this.open(conversation.id, false);
		void this.refreshConversations();
	}

	#dropped(runId: string) {
		this.#closeStream();
		if (this.#disposed) return;
		const running = this.active?.runs.some((run) => run.id === runId && isRunActive(run.status));
		if (running) this.#retryLater(runId);
	}

	#retryLater(runId: string) {
		if (this.#attempts >= MAX_RECONNECTS) {
			this.connection = 'lost';
			return;
		}
		this.#attempts += 1;
		this.connection = 'reconnecting';
		clearTimeout(this.#retry);
		this.#retry = setTimeout(() => {
			if (this.activeRun?.id === runId) void this.resume();
		}, 1000 * this.#attempts);
	}

	/** Ticks once a second while a task runs, so elapsed times stay current. */
	#syncClock() {
		if (this.activeRun && !this.#disposed) {
			if (this.#clock === undefined) {
				this.#clock = setInterval(() => (this.now = Date.now()), 1000);
			}
			this.now = Date.now();
			return;
		}
		clearInterval(this.#clock);
		this.#clock = undefined;
	}
}
