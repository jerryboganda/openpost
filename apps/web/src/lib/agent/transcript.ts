import {
	isRunActive,
	type AgentConversation,
	type AgentEvent,
	type AgentMessage,
	type AgentRun
} from './types';

type ToolState = 'running' | 'ok' | 'error' | 'stopped';

/** The tool arguments the page shows in a readable way. Others show as text. */
interface ToolArguments {
	path?: string;
	branch?: string;
	old_string?: string;
	new_string?: string;
	content?: string;
	sql?: string;
	method?: string;
}

export interface ToolStep {
	kind: 'tool';
	id: string;
	key: string;
	name: string;
	label: string;
	args: ToolArguments;
	input: string;
	state: ToolState;
	output: string;
	durationMs: number | null;
}

interface TextBlock {
	kind: 'text';
	id: string;
	text: string;
}

interface NoteBlock {
	kind: 'note';
	id: string;
	tone: 'info' | 'error';
	text: string;
}

type TurnBlock = ToolStep | TextBlock | NoteBlock;

/** One administrator message and everything the agent did in answer. */
export interface Turn {
	id: string;
	user: AgentMessage | null;
	run: AgentRun | null;
	blocks: TurnBlock[];
	/** Text of the model turn that is still streaming. */
	live: string;
	active: boolean;
}

const INPUT_LIMIT = 4000;

function parseText(value: unknown): string {
	return typeof value === 'string' ? value : '';
}

function parseDuration(value: unknown): number | null {
	return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
}

function parseOptionalText(value: unknown): string | undefined {
	return typeof value === 'string' && value !== '' ? value : undefined;
}

function parseToolArguments(value: unknown): ToolArguments {
	if (typeof value !== 'object' || value === null) return {};
	return {
		path: 'path' in value ? parseOptionalText(value.path) : undefined,
		branch: 'branch' in value ? parseOptionalText(value.branch) : undefined,
		old_string: 'old_string' in value ? parseOptionalText(value.old_string) : undefined,
		new_string: 'new_string' in value ? parseOptionalText(value.new_string) : undefined,
		content: 'content' in value ? parseOptionalText(value.content) : undefined,
		sql: 'sql' in value ? parseOptionalText(value.sql) : undefined,
		method: 'method' in value ? parseOptionalText(value.method) : undefined
	};
}

function clipInput(text: string): string {
	return text.length > INPUT_LIMIT ? `${text.slice(0, INPUT_LIMIT)}…` : text;
}

function parseToolInput(value: unknown): string {
	if (typeof value !== 'object' || value === null) return '';
	if ('truncated' in value && typeof value.truncated === 'string') return clipInput(value.truncated);
	const text = JSON.stringify(value, null, 2);
	return text === '{}' ? '' : clipInput(text);
}

function startsRun(event: AgentEvent): boolean {
	return Array.isArray(event.data?.tools);
}

function toolFromCall(event: AgentEvent, finished: boolean): ToolStep {
	const name = parseText(event.data?.name);
	return {
		kind: 'tool',
		id: event.id,
		key: parseText(event.data?.key) || event.id,
		name,
		label: event.message || name,
		args: parseToolArguments(event.data?.arguments),
		input: parseToolInput(event.data?.arguments),
		state: finished ? 'stopped' : 'running',
		output: '',
		durationMs: null
	};
}

function applyToolResult(step: ToolStep, event: AgentEvent) {
	step.state = event.data?.ok === false ? 'error' : 'ok';
	step.output = parseText(event.data?.output) || event.message;
	step.durationMs = parseDuration(event.data?.ms);
}

function buildBlocks(run: AgentRun | null, events: AgentEvent[]): TurnBlock[] {
	const finished = run ? !isRunActive(run.status) : true;
	const blocks: TurnBlock[] = [];
	const tools = new Map<string, ToolStep>();
	for (const event of events) {
		if (event.kind === 'text') {
			if (event.message.trim() !== '') blocks.push({ kind: 'text', id: event.id, text: event.message });
		} else if (event.kind === 'tool_call') {
			const step = toolFromCall(event, finished);
			tools.set(step.key, step);
			blocks.push(step);
		} else if (event.kind === 'tool_result') {
			const key = parseText(event.data?.key);
			let step = tools.get(key);
			if (!step) {
				step = toolFromCall(event, true);
				step.label = parseText(event.data?.name) || event.message;
				tools.set(step.key, step);
				blocks.push(step);
			}
			applyToolResult(step, event);
		} else if (event.kind === 'error') {
			blocks.push({ kind: 'note', id: event.id, tone: 'error', text: event.message });
		} else if (!startsRun(event) && event.message.trim() !== '') {
			blocks.push({ kind: 'note', id: event.id, tone: 'info', text: event.message });
		}
	}
	return blocks;
}

function lastTextBlock(blocks: TurnBlock[]): TextBlock | null {
	for (let index = blocks.length - 1; index >= 0; index -= 1) {
		const block = blocks[index];
		if (block.kind === 'text') return block;
	}
	return null;
}

/**
 * The final answer is stored as a message and was also streamed as the last
 * text event. Show it once. The stored message wins, because it can carry a
 * note that the event does not.
 */
function addFinalAnswer(blocks: TurnBlock[], answer: AgentMessage | undefined) {
	const content = answer?.content.trim() ?? '';
	if (!answer || content === '') return;
	const last = lastTextBlock(blocks);
	if (last && content.startsWith(last.text.trim())) {
		last.text = content;
		return;
	}
	blocks.push({ kind: 'text', id: `answer-${answer.id}`, text: content });
}

function addFailure(blocks: TurnBlock[], run: AgentRun | null) {
	const message = run?.status === 'failed' ? (run.failure_message ?? '').trim() : '';
	if (!run || message === '') return;
	const shown = blocks.some((block) => block.kind === 'note' && block.text === message);
	if (!shown) blocks.push({ kind: 'note', id: `failure-${run.id}`, tone: 'error', text: message });
}

function groupEvents(events: AgentEvent[]): Map<string, AgentEvent[]> {
	const grouped = new Map<string, AgentEvent[]>();
	for (const event of events) {
		const list = grouped.get(event.run_id);
		if (list) list.push(event);
		else grouped.set(event.run_id, [event]);
	}
	for (const list of grouped.values()) list.sort((left, right) => left.seq - right.seq);
	return grouped;
}

/** Turns a stored conversation into the turns the page renders. */
export function buildTranscript(conversation: AgentConversation | null, liveText: string): Turn[] {
	if (!conversation) return [];
	const runs = new Map(conversation.runs.map((run) => [run.id, run]));
	const events = groupEvents(conversation.events);
	const answers = new Map<string, AgentMessage>();
	for (const message of conversation.messages) {
		if (message.role === 'assistant' && message.run_id && runs.has(message.run_id)) {
			answers.set(message.run_id, message);
		}
	}

	const turns: Turn[] = [];
	for (const message of conversation.messages) {
		if (message.role === 'assistant') {
			if (message.run_id && runs.has(message.run_id)) continue;
			turns.push({
				id: message.id,
				user: null,
				run: null,
				blocks: [{ kind: 'text', id: message.id, text: message.content }],
				live: '',
				active: false
			});
			continue;
		}
		const run = (message.run_id ? runs.get(message.run_id) : undefined) ?? null;
		const blocks = buildBlocks(run, run ? (events.get(run.id) ?? []) : []);
		if (run) {
			addFinalAnswer(blocks, answers.get(run.id));
			addFailure(blocks, run);
		}
		turns.push({
			id: message.id,
			user: message,
			run,
			blocks,
			live: '',
			active: run ? isRunActive(run.status) : false
		});
	}
	const active = turns.findLast((turn) => turn.active);
	if (active) active.live = liveText;
	return turns;
}

/** How long a run has worked, or worked in total once it has finished. */
export function runDurationMs(run: AgentRun, now: number): number {
	const started = Date.parse(run.started_at ?? run.created_at);
	const ended = run.finished_at ? Date.parse(run.finished_at) : now;
	return Number.isNaN(started) || Number.isNaN(ended) ? 0 : Math.max(0, ended - started);
}
