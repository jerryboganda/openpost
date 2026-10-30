import { describe, expect, it } from 'vitest';
import { formatCount, formatDuration, formatRelativeTime } from './format';

describe('formatDuration', () => {
	it('shows tenths under a second and whole units above', () => {
		expect(formatDuration(0)).toBe('0.1s');
		expect(formatDuration(400)).toBe('0.4s');
		expect(formatDuration(949)).toBe('0.9s');
		expect(formatDuration(950)).toBe('1s');
		expect(formatDuration(12_400)).toBe('12s');
		expect(formatDuration(125_000)).toBe('2m 05s');
		expect(formatDuration(3_780_000)).toBe('1h 03m');
	});

	it('returns nothing for values that are not durations', () => {
		expect(formatDuration(-1)).toBe('');
		expect(formatDuration(Number.NaN)).toBe('');
	});
});

describe('formatCount', () => {
	it('abbreviates large counts', () => {
		expect(formatCount(999, 'en')).toBe('999');
		expect(formatCount(12_400, 'en')).toBe('12.4K');
		expect(formatCount(1_500_000, 'en')).toBe('1.5M');
	});
});

describe('formatRelativeTime', () => {
	const now = Date.parse('2026-10-30T12:00:00Z');

	it('describes how long ago something happened', () => {
		expect(formatRelativeTime('2026-10-30T11:59:50Z', now, 'en')).toBe('now');
		expect(formatRelativeTime('2026-10-30T11:55:00Z', now, 'en')).toBe('5 minutes ago');
		expect(formatRelativeTime('2026-10-30T09:00:00Z', now, 'en')).toBe('3 hours ago');
		expect(formatRelativeTime('2026-10-29T12:00:00Z', now, 'en')).toBe('yesterday');
	});

	it('returns nothing for an invalid time', () => {
		expect(formatRelativeTime('not a time', now, 'en')).toBe('');
	});
});
