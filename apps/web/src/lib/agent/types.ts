import type { components } from '$lib/api/types';

type Schemas = components['schemas'];

export type AgentEngine = Schemas['EngineStatus'];
export type AgentModel = Schemas['ModelOption'];
export type AgentConversationSummary = Schemas['AgentConversation'];
type AgentConversationResponse = Schemas['AdminAgentConversationResponse'];
export type AgentMessage = Schemas['AgentMessage'];
export type AgentRun = Schemas['AgentRun'];
export type AgentEvent = Schemas['AgentEvent'];
export type AgentAttachment = Schemas['AttachmentInfo'];

/** A conversation as the page uses it: the API sends empty lists as null. */
export interface AgentConversation extends Omit<
	AgentConversationResponse,
	'messages' | 'runs' | 'events'
> {
	messages: AgentMessage[];
	runs: AgentRun[];
	events: AgentEvent[];
}

export type AgentEffort = 'auto' | 'none' | 'minimal' | 'low' | 'medium' | 'high' | 'xhigh' | 'max';
export type AgentAccess = 'full' | 'read_only';

export const BUILT_IN_ENGINE = 'assistant';

export function normalizeConversation(response: AgentConversationResponse): AgentConversation {
	return {
		...response,
		messages: response.messages ?? [],
		runs: response.runs ?? [],
		events: response.events ?? []
	};
}

export function isRunActive(status: string): boolean {
	return status === 'queued' || status === 'dispatched' || status === 'running';
}
