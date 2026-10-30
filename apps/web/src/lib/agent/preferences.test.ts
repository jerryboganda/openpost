import { describe, expect, it } from 'vitest';
import { effortsFor, fitEffort, normalizePreferences } from './preferences';
import type { AgentModel } from './types';

const models: AgentModel[] = [
	{
		id: 'deepseek-v4.1-flash',
		label: 'DeepSeek V4.1 Flash',
		description: '',
		vision: true,
		efforts: ['none', 'low', 'high', 'max']
	},
	{
		id: 'glm-5.3',
		label: 'GLM 5.3',
		description: '',
		vision: false,
		efforts: ['low', 'high', 'max']
	}
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

	it('keeps a saved effort even when the model offers less, so it can return with the model', () => {
		expect(
			normalizePreferences({ model: 'glm-5.3', effort: 'none' }, models, 'deepseek-v4.1-flash')
		).toEqual({ model: 'glm-5.3', effort: 'none', access: 'full' });
	});

	it('falls back to the default model when the saved one is gone', () => {
		expect(normalizePreferences({ model: 'retired-model' }, models, 'deepseek-v4.1-flash')).toEqual(
			{
				model: 'deepseek-v4.1-flash',
				effort: 'auto',
				access: 'full'
			}
		);
	});

	it('starts with automatic reasoning and full access when nothing is saved', () => {
		expect(normalizePreferences({}, models, 'deepseek-v4.1-flash')).toEqual({
			model: 'deepseek-v4.1-flash',
			effort: 'auto',
			access: 'full'
		});
	});
});

describe('effortsFor', () => {
	it('lists Automatic first and then only what the model offers, lowest first', () => {
		expect(effortsFor(models[0])).toEqual(['auto', 'none', 'low', 'high', 'max']);
		expect(effortsFor(models[1])).toEqual(['auto', 'low', 'high', 'max']);
	});

	it('offers only Automatic when the model has nothing to choose or is not known yet', () => {
		expect(effortsFor({ ...models[1], efforts: [] })).toEqual(['auto']);
		expect(effortsFor({ ...models[1], efforts: null })).toEqual(['auto']);
		expect(effortsFor(null)).toEqual(['auto']);
		expect(effortsFor(undefined)).toEqual(['auto']);
	});

	it('ignores efforts this page does not know', () => {
		expect(effortsFor({ ...models[1], efforts: ['turbo', 'low'] })).toEqual(['auto', 'low']);
	});
});

describe('fitEffort', () => {
	const flash = effortsFor(models[0]);
	const glm = effortsFor(models[1]);

	it('keeps a choice the model offers', () => {
		expect(fitEffort('max', flash)).toBe('max');
		expect(fitEffort('none', flash)).toBe('none');
		expect(fitEffort('auto', glm)).toBe('auto');
	});

	it('steps down to the highest effort the model offers', () => {
		expect(fitEffort('xhigh', flash)).toBe('high');
		expect(fitEffort('medium', flash)).toBe('low');
		expect(fitEffort('minimal', flash)).toBe('none');
	});

	it('steps up only when nothing lower is offered', () => {
		expect(fitEffort('none', glm)).toBe('low');
		expect(fitEffort('minimal', glm)).toBe('low');
	});

	it('falls back to Automatic when the model has nothing to choose', () => {
		expect(fitEffort('max', ['auto'])).toBe('auto');
		expect(fitEffort('low', [])).toBe('auto');
	});
});
