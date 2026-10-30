import { describe, expect, it } from 'vitest';
import {
	ImageRejected,
	MAX_IMAGE_BYTES,
	compressedName,
	fitWithin,
	isAcceptedImage,
	needsCompression
} from './image-policy';

describe('isAcceptedImage', () => {
	it('accepts the formats the server reads and nothing else', () => {
		for (const type of ['image/png', 'image/jpeg', 'image/gif', 'image/webp']) {
			expect(isAcceptedImage(type)).toBe(true);
		}
		for (const type of ['image/svg+xml', 'image/bmp', 'application/pdf', 'text/html', '']) {
			expect(isAcceptedImage(type)).toBe(false);
		}
	});
});

describe('needsCompression', () => {
	it('re-encodes large still images but never animated ones', () => {
		expect(needsCompression('image/png', 500_000)).toBe(false);
		expect(needsCompression('image/png', MAX_IMAGE_BYTES)).toBe(true);
		expect(needsCompression('image/jpeg', MAX_IMAGE_BYTES * 2)).toBe(true);
		expect(needsCompression('image/gif', MAX_IMAGE_BYTES * 2)).toBe(false);
	});
});

describe('fitWithin', () => {
	it('scales the longest edge down and keeps the ratio', () => {
		expect(fitWithin(4800, 2400, 2400)).toEqual({ width: 2400, height: 1200 });
		expect(fitWithin(1000, 3000, 1500)).toEqual({ width: 500, height: 1500 });
	});

	it('never enlarges and never reaches zero', () => {
		expect(fitWithin(800, 600, 2400)).toEqual({ width: 800, height: 600 });
		expect(fitWithin(10_000, 1, 100)).toEqual({ width: 100, height: 1 });
	});
});

describe('compressedName', () => {
	it('swaps the extension for jpg', () => {
		expect(compressedName('Screenshot 2026-10-30.png')).toBe('Screenshot 2026-10-30.jpg');
		expect(compressedName('archive.tar.webp')).toBe('archive.tar.jpg');
		expect(compressedName('noextension')).toBe('noextension.jpg');
		expect(compressedName('')).toBe('image.jpg');
	});
});

describe('ImageRejected', () => {
	it('carries the problem and the server message', () => {
		const error = new ImageRejected('upload', 'image is larger than 4 MB');
		expect(error.problem).toBe('upload');
		expect(error.detail).toBe('image is larger than 4 MB');
		expect(error.message).toBe('image is larger than 4 MB');
		expect(new ImageRejected('type').message).toBe('type');
	});
});
