import { applyAPIRequestHeaders } from '$lib/api/client';
import {
	COMPRESSION_STEPS,
	ImageRejected,
	MAX_IMAGE_BYTES,
	compressedName,
	fitWithin,
	isAcceptedImage,
	needsCompression,
	type ImageSize
} from './image-policy';
import type { AgentAttachment } from './types';

async function encodeJpeg(
	bitmap: ImageBitmap,
	size: ImageSize,
	quality: number
): Promise<Blob | null> {
	const canvas = document.createElement('canvas');
	canvas.width = size.width;
	canvas.height = size.height;
	const context = canvas.getContext('2d');
	if (!context) return null;
	// JPEG has no transparency, so transparent screenshots get a white page.
	context.fillStyle = '#ffffff';
	context.fillRect(0, 0, size.width, size.height);
	context.drawImage(bitmap, 0, 0, size.width, size.height);
	return new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, 'image/jpeg', quality));
}

/** Returns the file to upload, re-encoded when it is too large to send as it is. */
export async function prepareImage(file: File): Promise<File> {
	if (!isAcceptedImage(file.type)) throw new ImageRejected('type');
	if (!needsCompression(file.type, file.size)) {
		if (file.size > MAX_IMAGE_BYTES) throw new ImageRejected('too_large');
		return file;
	}
	let bitmap: ImageBitmap;
	try {
		bitmap = await createImageBitmap(file);
	} catch {
		throw new ImageRejected('unreadable');
	}
	try {
		for (const step of COMPRESSION_STEPS) {
			const size = fitWithin(bitmap.width, bitmap.height, step.edge);
			const blob = await encodeJpeg(bitmap, size, step.quality);
			if (blob && blob.size <= MAX_IMAGE_BYTES) {
				return new File([blob], compressedName(file.name), { type: 'image/jpeg' });
			}
		}
	} finally {
		bitmap.close();
	}
	throw new ImageRejected('too_large');
}

function parseProblemDetail(body: string): string {
	try {
		const parsed = JSON.parse(body);
		if (typeof parsed?.error === 'string') return parsed.error;
		if (typeof parsed?.detail === 'string') return parsed.detail;
	} catch {
		// A response that is not JSON has no server message to show.
	}
	return '';
}

function parseAttachment(value: unknown): AgentAttachment | null {
	if (typeof value !== 'object' || value === null) return null;
	if (!('id' in value) || typeof value.id !== 'string' || value.id === '') return null;
	if (!('filename' in value) || typeof value.filename !== 'string') return null;
	if (!('content_type' in value) || typeof value.content_type !== 'string') return null;
	if (!('size_bytes' in value) || typeof value.size_bytes !== 'number') return null;
	if (!('width' in value) || typeof value.width !== 'number') return null;
	if (!('height' in value) || typeof value.height !== 'number') return null;
	if (!('created_at' in value) || typeof value.created_at !== 'string') return null;
	return {
		id: value.id,
		filename: value.filename,
		content_type: value.content_type,
		size_bytes: value.size_bytes,
		width: value.width,
		height: value.height,
		created_at: value.created_at
	};
}

/** Uploads one image to the agent. It waits there until a message uses it. */
export async function uploadImage(file: File, signal?: AbortSignal): Promise<AgentAttachment> {
	const headers = applyAPIRequestHeaders(new Headers({ 'Content-Type': file.type }));
	let response: Response;
	try {
		response = await fetch(
			`/api/v1/admin/agent/attachments?filename=${encodeURIComponent(file.name)}`,
			{ method: 'POST', headers, body: file, signal }
		);
	} catch (error) {
		if (signal?.aborted) throw error;
		throw new ImageRejected('upload');
	}
	const body = await response.text();
	if (!response.ok) {
		const problem = response.status === 413 ? 'too_large' : 'upload';
		throw new ImageRejected(problem, parseProblemDetail(body));
	}
	let payload: unknown;
	try {
		payload = JSON.parse(body);
	} catch {
		throw new ImageRejected('upload');
	}
	const attachment = parseAttachment(payload);
	if (!attachment) throw new ImageRejected('upload');
	return attachment;
}

export function attachmentUrl(id: string): string {
	return `/api/v1/admin/agent/attachments/${encodeURIComponent(id)}`;
}
