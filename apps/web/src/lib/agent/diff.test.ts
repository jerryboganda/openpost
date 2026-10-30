import { describe, expect, it } from 'vitest';
import { countChanges, diffLines, foldContext } from './diff';

describe('diffLines', () => {
	it('marks a replaced line and keeps the unchanged lines as context', () => {
		expect(diffLines('a\nb\nc', 'a\nB\nc')).toEqual([
			{ kind: 'context', text: 'a' },
			{ kind: 'remove', text: 'b' },
			{ kind: 'add', text: 'B' },
			{ kind: 'context', text: 'c' }
		]);
	});

	it('shows an inserted line without touching its neighbors', () => {
		expect(diffLines('a\nc', 'a\nb\nc')).toEqual([
			{ kind: 'context', text: 'a' },
			{ kind: 'add', text: 'b' },
			{ kind: 'context', text: 'c' }
		]);
	});

	it('shows a deleted line and handles empty sides and identical text', () => {
		expect(diffLines('a\nb\nc', 'a\nc')).toEqual([
			{ kind: 'context', text: 'a' },
			{ kind: 'remove', text: 'b' },
			{ kind: 'context', text: 'c' }
		]);
		expect(diffLines('', 'x\ny')).toEqual([
			{ kind: 'add', text: 'x' },
			{ kind: 'add', text: 'y' }
		]);
		expect(diffLines('same\ntext', 'same\ntext').every((line) => line.kind === 'context')).toBe(
			true
		);
	});

	it('aligns a multi-line edit on the lines it keeps', () => {
		const lines = diffLines(
			'func run() {\n\tstep1()\n\tstep2()\n\tstep3()\n}',
			'func run() {\n\tstep1()\n\tstepTwo()\n\tstep3()\n\tstep4()\n}'
		);
		expect(countChanges(lines)).toEqual({ added: 2, removed: 1 });
		expect(lines.filter((line) => line.kind === 'context').map((line) => line.text)).toEqual([
			'func run() {',
			'\tstep1()',
			'\tstep3()',
			'}'
		]);
	});

	it('falls back to removing and adding blocks that are too large to compare', () => {
		const before = Array.from({ length: 700 }, (_, index) => `old ${index}`).join('\n');
		const after = Array.from({ length: 700 }, (_, index) => `new ${index}`).join('\n');
		expect(countChanges(diffLines(before, after))).toEqual({ added: 700, removed: 700 });
	});
});

describe('foldContext', () => {
	it('keeps three lines around a change and folds the rest with their count', () => {
		const lines = diffLines(
			[
				...Array.from({ length: 10 }, (_, index) => `top ${index}`),
				'old',
				...Array.from({ length: 10 }, (_, index) => `bottom ${index}`)
			].join('\n'),
			[
				...Array.from({ length: 10 }, (_, index) => `top ${index}`),
				'new',
				...Array.from({ length: 10 }, (_, index) => `bottom ${index}`)
			].join('\n')
		);
		const folded = foldContext(lines);
		expect(folded.map((line) => line.kind)).toEqual([
			'gap',
			'context',
			'context',
			'context',
			'remove',
			'add',
			'context',
			'context',
			'context',
			'gap'
		]);
		expect(folded[0]).toMatchObject({ hidden: 7 });
		expect(folded[folded.length - 1]).toMatchObject({ hidden: 7 });
	});

	it('returns the lines unchanged when nothing changed', () => {
		const lines = diffLines('a\nb', 'a\nb');
		expect(foldContext(lines)).toEqual(lines);
	});
});
