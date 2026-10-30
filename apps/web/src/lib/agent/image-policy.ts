export const MAX_IMAGES = 5;
export const MAX_IMAGE_BYTES = 4 * 1024 * 1024;
const ACCEPTED_IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const IMAGE_ACCEPT = ACCEPTED_IMAGE_TYPES.join(',');

// Images above this size are re-encoded before upload, so a full-resolution
// screenshot does not hit the server limit.
const COMPRESS_ABOVE_BYTES = Math.floor(MAX_IMAGE_BYTES * 0.85);

export const COMPRESSION_STEPS = [
	{ edge: 2400, quality: 0.86 },
	{ edge: 1800, quality: 0.78 },
	{ edge: 1280, quality: 0.7 }
];

export type ImageProblem = 'type' | 'too_large' | 'unreadable' | 'upload';

export class ImageRejected extends Error {
	readonly problem: ImageProblem;
	readonly detail: string;

	constructor(problem: ImageProblem, detail = '') {
		super(detail || problem);
		this.name = 'ImageRejected';
		this.problem = problem;
		this.detail = detail;
	}
}

export function isAcceptedImage(type: string): boolean {
	return ACCEPTED_IMAGE_TYPES.includes(type);
}

/** Whether an image should be re-encoded before it is uploaded. GIFs keep their frames. */
export function needsCompression(type: string, size: number): boolean {
	return size > COMPRESS_ABOVE_BYTES && type !== 'image/gif';
}

/** Scales a size down so its longest edge fits, never up. */
export function fitWithin(
	width: number,
	height: number,
	edge: number
): { width: number; height: number } {
	const longest = Math.max(width, height);
	if (longest <= edge) return { width, height };
	const scale = edge / longest;
	return {
		width: Math.max(1, Math.round(width * scale)),
		height: Math.max(1, Math.round(height * scale))
	};
}

/** shot.png becomes shot.jpg. */
export function compressedName(name: string): string {
	const base = name.replace(/\.[^./\\]+$/, '');
	return `${base || 'image'}.jpg`;
}
