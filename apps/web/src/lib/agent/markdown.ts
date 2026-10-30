// A small Markdown parser for agent replies. It produces a tree that Svelte
// renders as elements, never HTML, so reply text cannot inject markup. It
// accepts incomplete input, because replies stream in: an unclosed code fence
// becomes a code block that grows until it closes.

export type InlineNode =
	| { type: 'text'; text: string }
	| { type: 'code'; text: string }
	| { type: 'strong'; children: InlineNode[] }
	| { type: 'em'; children: InlineNode[] }
	| { type: 'del'; children: InlineNode[] }
	| { type: 'link'; href: string; children: InlineNode[] }
	| { type: 'break' };

type TableAlignment = 'left' | 'center' | 'right' | null;

export type BlockNode =
	| { type: 'paragraph'; children: InlineNode[] }
	| { type: 'heading'; level: number; children: InlineNode[] }
	| { type: 'code'; lang: string; text: string; closed: boolean }
	| { type: 'list'; ordered: boolean; start: number; items: BlockNode[][] }
	| { type: 'quote'; children: BlockNode[] }
	| { type: 'table'; align: TableAlignment[]; head: InlineNode[][]; rows: InlineNode[][][] }
	| { type: 'rule' };

const FENCE_OPEN = /^( {0,3})(`{3,}|~{3,})[ \t]*([^\s`]*)[^`]*$/;
const HEADING = /^ {0,3}(#{1,6})[ \t]+(.*?)(?:[ \t]+#+)?[ \t]*$/;
const RULE = /^ {0,3}([-*_])(?:[ \t]*\1){2,}[ \t]*$/;
const QUOTE = /^ {0,3}>[ \t]?(.*)$/;
const BULLET = /^( {0,3})([*+-])([ \t]+)(.*)$/;
const ORDERED = /^( {0,3})(\d{1,9})([.)])([ \t]+)(.*)$/;
const TABLE_DELIMITER = /^[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;
const ESCAPABLE = '\\`*_{}[]()#+-.!|~>';
const LETTER_OR_DIGIT = /[\p{L}\p{N}]/u;

export function parseMarkdown(source: string): BlockNode[] {
	return parseBlocks(source.replace(/\r\n?/g, '\n').split('\n'));
}

function isBlank(line: string): boolean {
	return line.trim() === '';
}

function leadingSpaces(line: string): number {
	let count = 0;
	while (count < line.length && line[count] === ' ') count += 1;
	return count;
}

interface ListMarker {
	ordered: boolean;
	start: number;
	indent: number;
	contentIndent: number;
	rest: string;
	delimiter: string;
}

function readListMarker(line: string): ListMarker | null {
	const bullet = BULLET.exec(line);
	if (bullet && !RULE.test(line)) {
		const indent = bullet[1].length;
		return {
			ordered: false,
			start: 1,
			indent,
			contentIndent: indent + 1 + Math.min(bullet[3].length, 4),
			rest: bullet[4],
			delimiter: bullet[2]
		};
	}
	const ordered = ORDERED.exec(line);
	if (ordered) {
		const indent = ordered[1].length;
		return {
			ordered: true,
			start: Number.parseInt(ordered[2], 10),
			indent,
			contentIndent: indent + ordered[2].length + 1 + Math.min(ordered[4].length, 4),
			rest: ordered[5],
			delimiter: ordered[3]
		};
	}
	return null;
}

function startsTable(lines: string[], index: number): boolean {
	if (!lines[index].includes('|') || index + 1 >= lines.length) return false;
	const delimiter = lines[index + 1];
	return (
		delimiter.includes('-') &&
		TABLE_DELIMITER.test(delimiter) &&
		splitTableRow(lines[index]).length === splitTableRow(delimiter).length
	);
}

function startsBlock(lines: string[], index: number): boolean {
	const line = lines[index];
	return (
		FENCE_OPEN.test(line) ||
		HEADING.test(line) ||
		RULE.test(line) ||
		QUOTE.test(line) ||
		readListMarker(line) !== null ||
		startsTable(lines, index)
	);
}

function parseBlocks(lines: string[]): BlockNode[] {
	const blocks: BlockNode[] = [];
	let index = 0;
	while (index < lines.length) {
		const line = lines[index];
		if (isBlank(line)) {
			index += 1;
			continue;
		}
		const fence = FENCE_OPEN.exec(line);
		if (fence) {
			index = readFence(lines, index, fence, blocks);
			continue;
		}
		const heading = HEADING.exec(line);
		if (heading) {
			blocks.push({
				type: 'heading',
				level: heading[1].length,
				children: parseInline(heading[2])
			});
			index += 1;
			continue;
		}
		if (RULE.test(line)) {
			blocks.push({ type: 'rule' });
			index += 1;
			continue;
		}
		if (QUOTE.test(line)) {
			index = readQuote(lines, index, blocks);
			continue;
		}
		if (readListMarker(line)) {
			index = readList(lines, index, blocks);
			continue;
		}
		if (startsTable(lines, index)) {
			index = readTable(lines, index, blocks);
			continue;
		}
		index = readParagraph(lines, index, blocks);
	}
	return blocks;
}

function readFence(
	lines: string[],
	start: number,
	opening: RegExpExecArray,
	blocks: BlockNode[]
): number {
	const indent = opening[1].length;
	const marker = opening[2];
	const body: string[] = [];
	let index = start + 1;
	let closed = false;
	while (index < lines.length) {
		const line = lines[index];
		const close = /^ {0,3}(`{3,}|~{3,})[ \t]*$/.exec(line);
		if (close && close[1][0] === marker[0] && close[1].length >= marker.length) {
			closed = true;
			index += 1;
			break;
		}
		body.push(line.slice(Math.min(indent, leadingSpaces(line))));
		index += 1;
	}
	blocks.push({ type: 'code', lang: opening[3].toLowerCase(), text: body.join('\n'), closed });
	return index;
}

function readQuote(lines: string[], start: number, blocks: BlockNode[]): number {
	const inner: string[] = [];
	let index = start;
	while (index < lines.length) {
		const match = QUOTE.exec(lines[index]);
		if (!match) break;
		inner.push(match[1]);
		index += 1;
	}
	blocks.push({ type: 'quote', children: parseBlocks(inner) });
	return index;
}

function readParagraph(lines: string[], start: number, blocks: BlockNode[]): number {
	const collected: string[] = [lines[start].trim()];
	let index = start + 1;
	while (index < lines.length && !isBlank(lines[index]) && !startsBlock(lines, index)) {
		collected.push(lines[index].trim());
		index += 1;
	}
	blocks.push({ type: 'paragraph', children: parseInline(collected.join('\n')) });
	return index;
}

function nextNonBlank(lines: string[], from: number): number {
	let index = from;
	while (index < lines.length && isBlank(lines[index])) index += 1;
	return index;
}

function readList(lines: string[], start: number, blocks: BlockNode[]): number {
	const first = readListMarker(lines[start]);
	if (!first) return start + 1;
	const items: BlockNode[][] = [];
	let index = start;
	while (index < lines.length) {
		const marker = readListMarker(lines[index]);
		if (
			!marker ||
			marker.ordered !== first.ordered ||
			marker.delimiter !== first.delimiter ||
			marker.indent > first.indent + 1 ||
			marker.indent < first.indent - 1
		) {
			break;
		}
		const itemLines = [marker.rest];
		index += 1;
		index = collectItemLines(lines, index, marker, itemLines);
		items.push(parseBlocks(itemLines));
		const following = nextNonBlank(lines, index);
		if (following >= lines.length) {
			index = following;
			break;
		}
		const nextMarker = readListMarker(lines[following]);
		if (nextMarker && nextMarker.ordered === first.ordered && following > index) {
			// A blank line between items keeps one list.
			index = following;
		}
	}
	blocks.push({ type: 'list', ordered: first.ordered, start: first.start, items });
	return index;
}

function collectItemLines(
	lines: string[],
	from: number,
	parent: ListMarker,
	itemLines: string[]
): number {
	const { contentIndent } = parent;
	let index = from;
	while (index < lines.length) {
		const line = lines[index];
		if (isBlank(line)) {
			const following = nextNonBlank(lines, index + 1);
			if (following < lines.length && leadingSpaces(lines[following]) >= contentIndent) {
				itemLines.push('');
				index += 1;
				continue;
			}
			break;
		}
		const spaces = leadingSpaces(line);
		// Models often indent a nested list by less than the parent's content
		// column, so any list line indented past the parent marker nests.
		const nestsLess = spaces > parent.indent && readListMarker(line) !== null;
		if (spaces >= contentIndent || nestsLess) {
			itemLines.push(line.slice(Math.min(contentIndent, spaces)));
			index += 1;
			continue;
		}
		const previousBlank = itemLines.length > 0 && isBlank(itemLines[itemLines.length - 1]);
		if (!previousBlank && !startsBlock(lines, index)) {
			itemLines.push(line.trim());
			index += 1;
			continue;
		}
		break;
	}
	return index;
}

function splitTableRow(line: string): string[] {
	let text = line.trim();
	if (text.startsWith('|')) text = text.slice(1);
	if (text.endsWith('|') && !text.endsWith('\\|')) text = text.slice(0, -1);
	const cells: string[] = [];
	let current = '';
	for (let index = 0; index < text.length; index += 1) {
		const char = text[index];
		if (char === '\\' && text[index + 1] === '|') {
			current += '|';
			index += 1;
		} else if (char === '|') {
			cells.push(current.trim());
			current = '';
		} else {
			current += char;
		}
	}
	cells.push(current.trim());
	return cells;
}

function tableAlignment(cell: string): TableAlignment {
	const left = cell.startsWith(':');
	const right = cell.endsWith(':');
	if (left && right) return 'center';
	if (left) return 'left';
	if (right) return 'right';
	return null;
}

function readTable(lines: string[], start: number, blocks: BlockNode[]): number {
	const head = splitTableRow(lines[start]);
	const align = splitTableRow(lines[start + 1]).map(tableAlignment);
	const columns = head.length;
	const rows: InlineNode[][][] = [];
	let index = start + 2;
	while (index < lines.length && !isBlank(lines[index]) && lines[index].includes('|')) {
		const cells = splitTableRow(lines[index]);
		const padded = Array.from({ length: columns }, (_, column) => cells[column] ?? '');
		rows.push(padded.map(parseInline));
		index += 1;
	}
	blocks.push({
		type: 'table',
		align: Array.from({ length: columns }, (_, column) => align[column] ?? null),
		head: head.map(parseInline),
		rows
	});
	return index;
}

interface Span {
	text: string;
	end: number;
}

function readCodeSpan(source: string, index: number): Span | null {
	let run = index;
	while (source[run] === '`') run += 1;
	const length = run - index;
	let search = run;
	while (search < source.length) {
		const open = source.indexOf('`', search);
		if (open < 0) return null;
		let close = open;
		while (source[close] === '`') close += 1;
		if (close - open === length) {
			let text = source.slice(run, open).replace(/\n/g, ' ');
			if (text.length > 1 && text.startsWith(' ') && text.endsWith(' ') && text.trim() !== '') {
				text = text.slice(1, -1);
			}
			return { text, end: close };
		}
		search = close;
	}
	return null;
}

interface LinkSpan {
	label: string;
	href: string;
	end: number;
}

function readLink(source: string, index: number): LinkSpan | null {
	let depth = 0;
	let cursor = index;
	for (; cursor < source.length; cursor += 1) {
		const char = source[cursor];
		if (char === '\\') {
			cursor += 1;
		} else if (char === '`') {
			const code = readCodeSpan(source, cursor);
			if (code) cursor = code.end - 1;
		} else if (char === '[') {
			depth += 1;
		} else if (char === ']') {
			depth -= 1;
			if (depth === 0) break;
		}
	}
	if (cursor >= source.length || source[cursor + 1] !== '(') return null;
	let destination = '';
	let parentheses = 1;
	let position = cursor + 2;
	for (; position < source.length; position += 1) {
		const char = source[position];
		if (char === '\n') return null;
		if (char === '\\' && position + 1 < source.length) {
			destination += source[position + 1];
			position += 1;
			continue;
		}
		if (char === '(') parentheses += 1;
		if (char === ')') {
			parentheses -= 1;
			if (parentheses === 0) break;
		}
		destination += char;
	}
	if (parentheses !== 0) return null;
	const target = destination.trim().split(/\s+/)[0] ?? '';
	const href = target.startsWith('<') && target.endsWith('>') ? target.slice(1, -1) : target;
	return { label: source.slice(index + 1, cursor), href, end: position + 1 };
}

/** Allows web and mail links and same-site paths. Anything else is plain text. */
function safeHref(href: string): string | null {
	const value = href.trim();
	if (value === '') return null;
	if (value.startsWith('#') || (value.startsWith('/') && !value.startsWith('//'))) return value;
	return /^(?:https?:\/\/|mailto:)[^\s]/i.test(value) ? value : null;
}

function findClosing(source: string, from: number, marker: string): number {
	const char = marker[0];
	for (let index = from; index < source.length; index += 1) {
		const current = source[index];
		if (current === '\\') {
			index += 1;
			continue;
		}
		if (current === '`') {
			const code = readCodeSpan(source, index);
			if (code) {
				index = code.end - 1;
				continue;
			}
		}
		if (current !== char) continue;
		let run = 1;
		while (source[index + run] === char) run += 1;
		const hugsText = index > from && !/\s/.test(source[index - 1]);
		const usable = marker.length === 1 ? run === 1 : run >= marker.length;
		if (usable && hugsText) {
			const after = source[index + marker.length];
			if (char === '_' && after !== undefined && LETTER_OR_DIGIT.test(after)) {
				index += run - 1;
				continue;
			}
			return index;
		}
		index += run - 1;
	}
	return -1;
}

function readEmphasis(source: string, index: number): { node: InlineNode; end: number } | null {
	const char = source[index];
	if (char === '_' && index > 0 && LETTER_OR_DIGIT.test(source[index - 1])) return null;
	const markers: string[] = [];
	if (char === '~') {
		if (source[index + 1] === '~') markers.push('~~');
	} else {
		if (source[index + 1] === char && source[index + 2] === char) markers.push(char.repeat(3));
		if (source[index + 1] === char) markers.push(char.repeat(2));
		markers.push(char);
	}
	for (const marker of markers) {
		const start = index + marker.length;
		if (start >= source.length || /\s/.test(source[start])) continue;
		const close = findClosing(source, start, marker);
		if (close < 0) continue;
		const children = parseInline(source.slice(start, close));
		const end = close + marker.length;
		if (marker === '~~') return { node: { type: 'del', children }, end };
		if (marker.length === 3) {
			return { node: { type: 'strong', children: [{ type: 'em', children }] }, end };
		}
		return { node: { type: marker.length === 2 ? 'strong' : 'em', children }, end };
	}
	return null;
}

function readBareUrl(source: string, index: number): Span | null {
	if (index > 0 && LETTER_OR_DIGIT.test(source[index - 1])) return null;
	const match = /^https?:\/\/[^\s<]+/i.exec(source.slice(index));
	if (!match) return null;
	let url = match[0];
	const count = (text: string, char: string) => text.split(char).length - 1;
	while (
		/[.,;:!?'"*_~]$/.test(url) ||
		(url.endsWith(')') && count(url, '(') < count(url, ')')) ||
		(url.endsWith(']') && count(url, '[') < count(url, ']'))
	) {
		url = url.slice(0, -1);
	}
	return url.length > 8 ? { text: url, end: index + url.length } : null;
}

function parseInline(source: string): InlineNode[] {
	const nodes: InlineNode[] = [];
	let buffer = '';
	const flush = () => {
		if (buffer) nodes.push({ type: 'text', text: buffer });
		buffer = '';
	};
	let index = 0;
	while (index < source.length) {
		const char = source[index];
		const next = source[index + 1];
		if (char === '\\' && next !== undefined && ESCAPABLE.includes(next)) {
			buffer += next;
			index += 2;
			continue;
		}
		if (char === '\n') {
			flush();
			nodes.push({ type: 'break' });
			index += 1;
			continue;
		}
		if (char === '`') {
			const code = readCodeSpan(source, index);
			if (code) {
				flush();
				nodes.push({ type: 'code', text: code.text });
				index = code.end;
				continue;
			}
			let run = index;
			while (source[run] === '`') run += 1;
			buffer += source.slice(index, run);
			index = run;
			continue;
		}
		if (char === '[' || (char === '!' && next === '[')) {
			const isImage = char === '!';
			const link = readLink(source, isImage ? index + 1 : index);
			if (link) {
				flush();
				const href = safeHref(link.href);
				const unlabeled = link.label.trim() === '';
				const children: InlineNode[] = unlabeled
					? [{ type: 'text', text: link.href }]
					: parseInline(link.label);
				if (href) nodes.push({ type: 'link', href, children });
				else nodes.push(...children);
				index = link.end;
				continue;
			}
		}
		if (char === '*' || char === '_' || char === '~') {
			const span = readEmphasis(source, index);
			if (span) {
				flush();
				nodes.push(span.node);
				index = span.end;
				continue;
			}
		}
		if (char === '<') {
			const auto = /^<(https?:\/\/[^\s<>]+)>/i.exec(source.slice(index));
			if (auto) {
				flush();
				nodes.push({ type: 'link', href: auto[1], children: [{ type: 'text', text: auto[1] }] });
				index += auto[0].length;
				continue;
			}
		}
		if (
			(char === 'h' || char === 'H') &&
			source.slice(index + 1, index + 4).toLowerCase() === 'ttp'
		) {
			const url = readBareUrl(source, index);
			if (url) {
				flush();
				nodes.push({ type: 'link', href: url.text, children: [{ type: 'text', text: url.text }] });
				index = url.end;
				continue;
			}
		}
		buffer += char;
		index += 1;
	}
	flush();
	return mergeAdjacentText(nodes);
}

function mergeAdjacentText(nodes: InlineNode[]): InlineNode[] {
	const merged: InlineNode[] = [];
	for (const node of nodes) {
		const last = merged[merged.length - 1];
		if (node.type === 'text' && last?.type === 'text') last.text += node.text;
		else merged.push(node);
	}
	return merged;
}
