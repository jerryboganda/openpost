import type { AgentAccess, AgentEffort, AgentModel } from './types';

export interface AgentPreferences {
	model: string;
	effort: AgentEffort;
	access: AgentAccess;
}

export const EFFORTS: AgentEffort[] = ['auto', 'low', 'medium', 'high'];

const PREFERENCES_KEY = 'openpost.agent.preferences';
const CONVERSATION_KEY = 'openpost.agent.conversation';

function parseEffort(value: unknown): AgentEffort | undefined {
	return EFFORTS.find((effort) => effort === value);
}

function parseAccess(value: unknown): AgentAccess | undefined {
	return value === 'full' || value === 'read_only' ? value : undefined;
}

/** Reads the saved choices of this browser. Anything unreadable is ignored. */
export function readPreferences(): Partial<AgentPreferences> {
	try {
		const raw = globalThis.localStorage?.getItem(PREFERENCES_KEY);
		if (!raw) return {};
		const parsed = JSON.parse(raw);
		return {
			model: typeof parsed?.model === 'string' ? parsed.model : undefined,
			effort: parseEffort(parsed?.effort),
			access: parseAccess(parsed?.access)
		};
	} catch {
		return {};
	}
}

export function storePreferences(preferences: AgentPreferences) {
	try {
		globalThis.localStorage?.setItem(PREFERENCES_KEY, JSON.stringify(preferences));
	} catch {
		// Private browsing and full storage only lose the saved choice.
	}
}

/** Fills the gaps in saved choices. A model the server no longer offers falls back to the default. */
export function normalizePreferences(
	saved: Partial<AgentPreferences>,
	models: AgentModel[],
	defaultModel: string
): AgentPreferences {
	const known = models.find((option) => option.id === saved.model);
	return {
		model: known?.id ?? defaultModel,
		effort: saved.effort ?? 'auto',
		access: saved.access ?? 'full'
	};
}

export function readLastConversation(): string {
	try {
		return globalThis.localStorage?.getItem(CONVERSATION_KEY) ?? '';
	} catch {
		return '';
	}
}

export function storeLastConversation(id: string) {
	try {
		if (id) globalThis.localStorage?.setItem(CONVERSATION_KEY, id);
		else globalThis.localStorage?.removeItem(CONVERSATION_KEY);
	} catch {
		// Private browsing and full storage only lose the shortcut back to this chat.
	}
}
