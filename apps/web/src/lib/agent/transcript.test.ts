import { describe, expect, it } from 'vitest';
import { buildTranscript, runDurationMs, type ToolStep, type Turn } from './transcript';
import type { AgentConversation, AgentEvent, AgentMessage, AgentRun } from './types';

const at = '2026-10-30T12:00:00Z';

function message(
	id: string,
	role: 'user' | 'assistant',
	content: string,
	runId?: string
): AgentMessage {
	return { id, conversation_id: 'c1', role, content, run_id: runId, created_at: at };
}

function run(id: string, status: string, extra: Partial<AgentRun> = {}): AgentRun {
	return {
		id,
		conversation_id: 'c1',
		engine: 'assistant',
		status,
		prompt: 'prompt',
		created_at: at,
		...extra
	};
}

function event(
	runId: string,
	seq: number,
	kind: string,
	text: string,
	data?: AgentEvent['data']
): AgentEvent {
	return { id: `${runId}-${seq}`, run_id: runId, seq, kind, message: text, data, created_at: at };
}

function conversation(parts: Partial<AgentConversation>): AgentConversation {
	return {
		id: 'c1',
		engine: 'assistant',
		title: 'Chat',
		created_at: at,
		updated_at: at,
		messages: [],
		runs: [],
		events: [],
		...parts
	};
}

type Block = Turn['blocks'][number];

function tools(blocks: Block[]): ToolStep[] {
	return blocks.filter((block): block is ToolStep => block.kind === 'tool');
}

function texts(blocks: Block[]): string[] {
	return blocks.flatMap((block) => (block.kind === 'text' ? [block.text] : []));
}

describe('buildTranscript', () => {
	it('pairs a tool call with its result and shows the final answer once', () => {
		const turns = buildTranscript(
			conversation({
				messages: [
					message('m1', 'user', 'Read a.go', 'r1'),
					message('m2', 'assistant', 'All done.', 'r1')
				],
				runs: [run('r1', 'succeeded', { model: 'kimi-k3' })],
				events: [
					event('r1', 1, 'status', 'Working with Kimi K3', { model: 'kimi-k3', tools: ['repo_read'] }),
					event('r1', 2, 'text', "I'll read it."),
					event('r1', 3, 'tool_call', 'Read a.go', {
						key: '1-0',
						name: 'repo_read',
						arguments: { path: 'a.go' }
					}),
					event('r1', 4, 'tool_result', 'file contents', {
						key: '1-0',
						name: 'repo_read',
						ok: true,
						output: 'file contents',
						ms: 420
					}),
					event('r1', 5, 'text', 'All done.')
				]
			}),
			''
		);

		expect(turns).toHaveLength(1);
		const [turn] = turns;
		expect(turn.user?.content).toBe('Read a.go');
		expect(turn.blocks.map((block) => block.kind)).toEqual(['text', 'tool', 'text']);
		const [step] = tools(turn.blocks);
		expect(step).toMatchObject({
			name: 'repo_read',
			label: 'Read a.go',
			state: 'ok',
			output: 'file contents',
			durationMs: 420,
			args: { path: 'a.go' }
		});
		expect(texts(turn.blocks)).toEqual(["I'll read it.", 'All done.']);
	});

	it('keeps a tool running until its result arrives and stops it with the run', () => {
		const events = [
			event('r1', 1, 'tool_call', 'List the repository', { key: '1-0', name: 'repo_tree', arguments: {} })
		];
		const running = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'List', 'r1')],
				runs: [run('r1', 'running')],
				events
			}),
			''
		);
		expect(tools(running[0].blocks)[0].state).toBe('running');

		const canceled = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'List', 'r1')],
				runs: [run('r1', 'canceled')],
				events
			}),
			''
		);
		expect(tools(canceled[0].blocks)[0].state).toBe('stopped');
	});

	it('shows a failed tool call with its error text', () => {
		const [turn] = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'Edit', 'r1')],
				runs: [run('r1', 'running')],
				events: [
					event('r1', 1, 'tool_call', 'Edit a.go on agent/x', {
						key: '1-0',
						name: 'repo_edit',
						arguments: { path: 'a.go', branch: 'agent/x', old_string: 'a', new_string: 'b' }
					}),
					event('r1', 2, 'tool_result', 'Error: old_string was not found', {
						key: '1-0',
						name: 'repo_edit',
						ok: false,
						output: 'Error: old_string was not found in a.go',
						ms: 90
					})
				]
			}),
			''
		);
		expect(tools(turn.blocks)[0]).toMatchObject({
			state: 'error',
			output: 'Error: old_string was not found in a.go',
			args: { old_string: 'a', new_string: 'b', branch: 'agent/x' }
		});
	});

	it('adds the stored answer when no text event carried it', () => {
		const [turn] = buildTranscript(
			conversation({
				engine: 'codex',
				messages: [message('m1', 'user', 'Ship it', 'r1'), message('m2', 'assistant', 'Opened PR 7.', 'r1')],
				runs: [run('r1', 'succeeded', { engine: 'codex' })],
				events: [event('r1', 1, 'progress', 'Installing the engine')]
			}),
			''
		);
		expect(turn.blocks).toEqual([
			{ kind: 'note', id: 'r1-1', tone: 'info', text: 'Installing the engine' },
			{ kind: 'text', id: 'answer-m2', text: 'Opened PR 7.' }
		]);
	});

	it('prefers the stored answer when it adds a note to the last text event', () => {
		const stored = "Result\n\n_The reply reached the model's output limit and may be cut off._";
		const [turn] = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'Go', 'r1'), message('m2', 'assistant', stored, 'r1')],
				runs: [run('r1', 'succeeded')],
				events: [event('r1', 1, 'text', 'Result')]
			}),
			''
		);
		expect(turn.blocks).toHaveLength(1);
		expect(turn.blocks[0]).toMatchObject({ kind: 'text', text: stored });
	});

	it('shows a failure once, whether or not an error event recorded it', () => {
		const failure = 'The AI provider rejected the request: Model is unavailable.';
		const recorded = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'Hi', 'r1')],
				runs: [run('r1', 'failed', { failure_message: failure })],
				events: [event('r1', 1, 'error', failure)]
			}),
			''
		);
		expect(recorded[0].blocks.filter((block) => block.kind === 'note')).toHaveLength(1);

		const unrecorded = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'Hi', 'r1')],
				runs: [run('r1', 'failed', { failure_message: failure })]
			}),
			''
		);
		expect(unrecorded[0].blocks).toEqual([
			{ kind: 'note', id: 'failure-r1', tone: 'error', text: failure }
		]);
	});

	it('attaches streaming text to the active turn only', () => {
		const turns = buildTranscript(
			conversation({
				messages: [
					message('m1', 'user', 'First', 'r1'),
					message('m2', 'assistant', 'Done.', 'r1'),
					message('m3', 'user', 'Second', 'r2')
				],
				runs: [run('r1', 'succeeded'), run('r2', 'running')]
			}),
			'Partial reply'
		);
		expect(turns.map((turn) => [turn.active, turn.live])).toEqual([
			[false, ''],
			[true, 'Partial reply']
		]);
	});

	it('orders events by sequence and keeps events of other runs apart', () => {
		const turns = buildTranscript(
			conversation({
				messages: [message('m1', 'user', 'A', 'r1'), message('m2', 'user', 'B', 'r2')],
				runs: [run('r1', 'succeeded'), run('r2', 'succeeded')],
				events: [
					event('r1', 2, 'text', 'second'),
					event('r2', 1, 'text', 'other run'),
					event('r1', 1, 'text', 'first')
				]
			}),
			''
		);
		expect(texts(turns[0].blocks)).toEqual(['first', 'second']);
		expect(texts(turns[1].blocks)).toEqual(['other run']);
	});

	it('shows an assistant message without a run as its own turn', () => {
		const turns = buildTranscript(
			conversation({ messages: [message('m1', 'assistant', 'Earlier reply')] }),
			''
		);
		expect(turns).toEqual([
			{
				id: 'm1',
				user: null,
				run: null,
				blocks: [{ kind: 'text', id: 'm1', text: 'Earlier reply' }],
				live: '',
				active: false
			}
		]);
	});

	it('returns nothing before a conversation is open', () => {
		expect(buildTranscript(null, 'ignored')).toEqual([]);
	});
});

describe('runDurationMs', () => {
	it('measures a finished run and a running one', () => {
		const finished = run('r1', 'succeeded', {
			started_at: '2026-10-30T12:00:00Z',
			finished_at: '2026-10-30T12:00:12Z'
		});
		expect(runDurationMs(finished, Date.parse('2026-10-30T13:00:00Z'))).toBe(12_000);
		const running = run('r2', 'running', { started_at: '2026-10-30T12:00:00Z' });
		expect(runDurationMs(running, Date.parse('2026-10-30T12:00:05Z'))).toBe(5_000);
		expect(runDurationMs(run('r3', 'queued'), Date.parse('2026-10-30T11:00:00Z'))).toBe(0);
	});
});
