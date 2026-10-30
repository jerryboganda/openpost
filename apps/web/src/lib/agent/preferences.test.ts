import { describe, expect, it } from 'vitest';
import { normalizePreferences } from './preferences';
import type { AgentModel } from './types';

const models: AgentModel[] = [
	{ id: 'deepseek-v4.1-flash', label: 'DeepSeek V4.1 Flash', description: '', vision: true },
	{ id: 'glm-5.3', label: 'GLM 5.3', description: '', vision: false }
];

describe('normalizePreferences', () => {
	it('keeps saved choices the server still offers', () => {
		expect(
			normalizePreferences(
				{ model: 'glm-5.3', effort: 'high', access: 'read_only' },
				models,
				'deepseek-v4.1-flash'
			)
		).toEqual({ model: 'glm-5.3', effort: 'high', access: 'read_only' });
	});

	it('falls back to the default model when the saved one is gone', () => {
		expect(normalizePreferences({ model: 'retired-model' }, models, 'deepseek-v4.1-flash')).toEqual({
			model: 'deepseek-v4.1-flash',
			effort: 'auto',
			access: 'full'
		});
	});

	it('starts with automatic reasoning and full access when nothing is saved', () => {
		expect(normalizePreferences({}, models, 'deepseek-v4.1-flash')).toEqual({
			model: 'deepseek-v4.1-flash',
			effort: 'auto',
			access: 'full'
		});
	});
});
