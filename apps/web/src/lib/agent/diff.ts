type DiffKind = 'context' | 'add' | 'remove';

type DiffLine =
	| { kind: DiffKind; text: string }
	| { kind: 'gap'; text: string; hidden: number };

// Beyond this many table cells the exact diff costs more than it helps, so the
// whole block shows as removed and added.
const MAX_TABLE_CELLS = 250_000;

function splitLines(text: string): string[] {
	if (text === '') return [];
	return text.replace(/\r\n?/g, '\n').split('\n');
}

function diffMiddle(before: string[], after: string[]): DiffLine[] {
	if (before.length * after.length > MAX_TABLE_CELLS) {
		return [
			...before.map((text): DiffLine => ({ kind: 'remove', text })),
			...after.map((text): DiffLine => ({ kind: 'add', text }))
		];
	}
	const widths = after.length + 1;
	const table = new Uint32Array((before.length + 1) * widths);
	for (let row = before.length - 1; row >= 0; row -= 1) {
		for (let column = after.length - 1; column >= 0; column -= 1) {
			table[row * widths + column] =
				before[row] === after[column]
					? table[(row + 1) * widths + column + 1] + 1
					: Math.max(table[(row + 1) * widths + column], table[row * widths + column + 1]);
		}
	}
	const lines: DiffLine[] = [];
	let row = 0;
	let column = 0;
	while (row < before.length && column < after.length) {
		if (before[row] === after[column]) {
			lines.push({ kind: 'context', text: before[row] });
			row += 1;
			column += 1;
		} else if (table[(row + 1) * widths + column] >= table[row * widths + column + 1]) {
			lines.push({ kind: 'remove', text: before[row] });
			row += 1;
		} else {
			lines.push({ kind: 'add', text: after[column] });
			column += 1;
		}
	}
	for (; row < before.length; row += 1) lines.push({ kind: 'remove', text: before[row] });
	for (; column < after.length; column += 1) lines.push({ kind: 'add', text: after[column] });
	return lines;
}

/** Line diff of two texts. Unchanged lines stay as context. */
export function diffLines(before: string, after: string): DiffLine[] {
	const left = splitLines(before);
	const right = splitLines(after);
	let start = 0;
	while (start < left.length && start < right.length && left[start] === right[start]) start += 1;
	let leftEnd = left.length;
	let rightEnd = right.length;
	while (leftEnd > start && rightEnd > start && left[leftEnd - 1] === right[rightEnd - 1]) {
		leftEnd -= 1;
		rightEnd -= 1;
	}
	return [
		...left.slice(0, start).map((text): DiffLine => ({ kind: 'context', text })),
		...diffMiddle(left.slice(start, leftEnd), right.slice(start, rightEnd)),
		...left.slice(leftEnd).map((text): DiffLine => ({ kind: 'context', text }))
	];
}

/** Keeps `keep` lines of context around each change and folds the rest. */
export function foldContext(lines: DiffLine[], keep = 3): DiffLine[] {
	const visible = lines.map((line) => line.kind !== 'context');
	const near = lines.map((_, index) => {
		for (let offset = -keep; offset <= keep; offset += 1) {
			if (visible[index + offset]) return true;
		}
		return false;
	});
	const folded: DiffLine[] = [];
	let hidden = 0;
	lines.forEach((line, index) => {
		if (near[index]) {
			if (hidden > 0) folded.push({ kind: 'gap', text: '', hidden });
			hidden = 0;
			folded.push(line);
		} else {
			hidden += 1;
		}
	});
	if (hidden > 0 && folded.length > 0) folded.push({ kind: 'gap', text: '', hidden });
	return folded.length > 0 ? folded : lines;
}

export function countChanges(lines: DiffLine[]): { added: number; removed: number } {
	let added = 0;
	let removed = 0;
	for (const line of lines) {
		if (line.kind === 'add') added += 1;
		if (line.kind === 'remove') removed += 1;
	}
	return { added, removed };
}
