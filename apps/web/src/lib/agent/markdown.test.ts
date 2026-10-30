import { describe, expect, it } from 'vitest';
import { parseMarkdown, type InlineNode } from './markdown';

const text = (value: string) => ({ type: 'text' as const, text: value });

/** The inline content of a source that is one paragraph. */
function inline(source: string): InlineNode[] {
	const [block] = parseMarkdown(source);
	if (block?.type !== 'paragraph') throw new Error(`expected one paragraph for ${source}`);
	return block.children;
}

describe('block parsing', () => {
	it('joins wrapped lines with a break and separates paragraphs by blank lines', () => {
		expect(parseMarkdown('First line\nSecond line\n\nNext paragraph')).toEqual([
			{ type: 'paragraph', children: [text('First line'), { type: 'break' }, text('Second line')] },
			{ type: 'paragraph', children: [text('Next paragraph')] }
		]);
	});

	it('reads headings and rules and ignores hashes that are not headings', () => {
		expect(parseMarkdown('## Plan\n\n---\n\n#hashtag')).toEqual([
			{ type: 'heading', level: 2, children: [text('Plan')] },
			{ type: 'rule' },
			{ type: 'paragraph', children: [text('#hashtag')] }
		]);
	});

	it('keeps fenced code literal and records whether the fence closed', () => {
		const closed = parseMarkdown('Intro\n\n```ts\nconst a = **1**;\n\n# not a heading\n```\n\nAfter');
		expect(closed).toEqual([
			{ type: 'paragraph', children: [text('Intro')] },
			{
				type: 'code',
				lang: 'ts',
				text: 'const a = **1**;\n\n# not a heading',
				closed: true
			},
			{ type: 'paragraph', children: [text('After')] }
		]);
		expect(parseMarkdown('Run:\n```bash\nnpm test')).toEqual([
			{ type: 'paragraph', children: [text('Run:')] },
			{ type: 'code', lang: 'bash', text: 'npm test', closed: false }
		]);
	});

	it('nests lists and keeps the first number of an ordered list', () => {
		expect(parseMarkdown('3. Three\n4. Four\n   - nested a\n   - nested b')).toEqual([
			{
				type: 'list',
				ordered: true,
				start: 3,
				items: [
					[{ type: 'paragraph', children: [text('Three')] }],
					[
						{ type: 'paragraph', children: [text('Four')] },
						{
							type: 'list',
							ordered: false,
							start: 1,
							items: [
								[{ type: 'paragraph', children: [text('nested a')] }],
								[{ type: 'paragraph', children: [text('nested b')] }]
							]
						}
					]
				]
			}
		]);
	});

	it('nests a list indented by less than the parent content column', () => {
		const [list] = parseMarkdown('1. Step\n  - sub');
		if (list?.type !== 'list') throw new Error('expected a list');
		expect(list.ordered).toBe(true);
		expect(list.items).toHaveLength(1);
		expect(list.items[0].map((block) => block.type)).toEqual(['paragraph', 'list']);
	});

	it('keeps items separated by a blank line in one list', () => {
		const [list, after] = parseMarkdown('- a\n\n- b\n\nDone');
		if (list?.type !== 'list') throw new Error('expected a list');
		expect(list.items).toHaveLength(2);
		expect(after).toEqual({ type: 'paragraph', children: [text('Done')] });
	});

	it('reads quotes with inline formatting', () => {
		expect(parseMarkdown('> quoted **text**\n> second\n\nafter')).toEqual([
			{
				type: 'quote',
				children: [
					{
						type: 'paragraph',
						children: [
							text('quoted '),
							{ type: 'strong', children: [text('text')] },
							{ type: 'break' },
							text('second')
						]
					}
				]
			},
			{ type: 'paragraph', children: [text('after')] }
		]);
	});

	it('reads tables with alignment, escaped pipes, and short rows', () => {
		expect(
			parseMarkdown('| Name | Count |\n| :--- | ---: |\n| a \\| b | 2 |\n| c |')
		).toEqual([
			{
				type: 'table',
				align: ['left', 'right'],
				head: [[text('Name')], [text('Count')]],
				rows: [
					[[text('a | b')], [text('2')]],
					[[text('c')], []]
				]
			}
		]);
	});

	it('does not read a table when the delimiter row has another column count', () => {
		expect(parseMarkdown('a | b\n---')).toEqual([
			{ type: 'paragraph', children: [text('a | b')] },
			{ type: 'rule' }
		]);
	});

	it('never produces markup from HTML in the source', () => {
		expect(parseMarkdown('<script>alert("x")</script>\n<img src=x onerror=alert(1)>')).toEqual([
			{
				type: 'paragraph',
				children: [
					text('<script>alert("x")</script>'),
					{ type: 'break' },
					text('<img src=x onerror=alert(1)>')
				]
			}
		]);
	});

	it('accepts every prefix of a reply, as it arrives while streaming', () => {
		const reply = [
			'## Result',
			'',
			'The **deploy** passed. See [run](https://example.com/run/1).',
			'',
			'| Job | State |',
			'| --- | --- |',
			'| build | ok |',
			'',
			'1. First',
			'   - sub item',
			'2. Second',
			'',
			'```go',
			'fmt.Println("done")',
			'```'
		].join('\n');
		for (let length = 0; length <= reply.length; length += 1) {
			expect(() => parseMarkdown(reply.slice(0, length))).not.toThrow();
		}
		expect(parseMarkdown(reply).map((block) => block.type)).toEqual([
			'heading',
			'paragraph',
			'table',
			'list',
			'code'
		]);
	});
});

describe('inline parsing', () => {
	it('reads emphasis, strikethrough, and code spans', () => {
		expect(inline('a **bold** and *it* and ~~gone~~ and `code`')).toEqual([
			text('a '),
			{ type: 'strong', children: [text('bold')] },
			text(' and '),
			{ type: 'em', children: [text('it')] },
			text(' and '),
			{ type: 'del', children: [text('gone')] },
			text(' and '),
			{ type: 'code', text: 'code' }
		]);
	});

	it('reads bold italic and nested emphasis', () => {
		expect(inline('***both*** and **bold with *it* inside**')).toEqual([
			{ type: 'strong', children: [{ type: 'em', children: [text('both')] }] },
			text(' and '),
			{
				type: 'strong',
				children: [text('bold with '), { type: 'em', children: [text('it')] }, text(' inside')]
			}
		]);
	});

	it('leaves identifiers, arithmetic, and unclosed markers as text', () => {
		expect(inline('use snake_case_name and 2 * 3 * 4')).toEqual([
			text('use snake_case_name and 2 * 3 * 4')
		]);
		expect(inline('an **unclosed marker')).toEqual([text('an **unclosed marker')]);
		expect(inline('a `lone backtick')).toEqual([text('a `lone backtick')]);
	});

	it('does not format inside code spans and honors escapes', () => {
		expect(inline('`a **b** c` and \\*literal\\*')).toEqual([
			{ type: 'code', text: 'a **b** c' },
			text(' and *literal*')
		]);
	});

	it('links safe targets and drops the link from unsafe ones', () => {
		expect(inline('[docs](https://example.com/a_b) and [bad](javascript:alert(1)) end')).toEqual([
			{ type: 'link', href: 'https://example.com/a_b', children: [text('docs')] },
			text(' and bad end')
		]);
		expect(inline('[agent](/agent) [mail](mailto:a@example.com)')).toEqual([
			{ type: 'link', href: '/agent', children: [text('agent')] },
			text(' '),
			{ type: 'link', href: 'mailto:a@example.com', children: [text('mail')] }
		]);
	});

	it('allows web, mail, anchor, and same-site targets only', () => {
		for (const unsafe of [
			'javascript:alert(1)',
			'data:text/html,<script>1</script>',
			'vbscript:x',
			'//evil.example/x',
			'apps/web/file.ts'
		]) {
			expect(inline(`[label](${unsafe})`)).toEqual([text('label')]);
		}
		for (const safe of ['https://example.com', 'HTTP://example.com', '#section', '/agent']) {
			expect(inline(`[label](${safe})`)).toEqual([
				{ type: 'link', href: safe, children: [text('label')] }
			]);
		}
	});

	it('links bare addresses without their trailing punctuation', () => {
		expect(inline('See https://example.com/path, then (https://example.com/x).')).toEqual([
			text('See '),
			{
				type: 'link',
				href: 'https://example.com/path',
				children: [text('https://example.com/path')]
			},
			text(' then ('),
			{ type: 'link', href: 'https://example.com/x', children: [text('https://example.com/x')] },
			text(').')
		]);
	});

	it('shows an image as a link to it and never loads it', () => {
		expect(inline('![chart](https://example.com/c.png)')).toEqual([
			{ type: 'link', href: 'https://example.com/c.png', children: [text('chart')] }
		]);
	});
});
