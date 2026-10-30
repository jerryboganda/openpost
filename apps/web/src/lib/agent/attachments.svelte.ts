import { ImageRejected, MAX_IMAGES, isAcceptedImage, type ImageProblem } from './image-policy';
import { prepareImage, uploadImage } from './images';
import type { AgentAttachment } from './types';

type PendingState = 'uploading' | 'ready' | 'failed';

export interface PendingImage {
	id: string;
	name: string;
	previewUrl: string;
	state: PendingState;
	attachment: AgentAttachment | null;
	problem: ImageProblem | null;
	detail: string;
}

/** Why the last attempt to add images dropped some of them. */
type TrayNotice = 'limit' | 'type' | null;

let nextId = 0;

/**
 * The images waiting to be sent with the next message. Each image uploads as
 * soon as it is added, so sending only has to wait for uploads that are still
 * running. Previews use object URLs, which the tray revokes when it lets go.
 */
export class AttachmentTray {
	items = $state<PendingImage[]>([]);
	notice = $state<TrayNotice>(null);

	ready = $derived(this.items.filter((item) => item.state === 'ready'));
	busy = $derived(this.items.some((item) => item.state === 'uploading'));
	failed = $derived(this.items.some((item) => item.state === 'failed'));
	full = $derived(this.items.length >= MAX_IMAGES);

	#files = new Map<string, File>();
	#uploads = new Map<string, AbortController>();

	add(files: File[]) {
		this.notice = null;
		for (const file of files) {
			if (!isAcceptedImage(file.type)) {
				this.notice = 'type';
				continue;
			}
			if (this.items.length >= MAX_IMAGES) {
				this.notice = 'limit';
				break;
			}
			nextId += 1;
			const id = `image-${nextId}`;
			this.#files.set(id, file);
			this.items.push({
				id,
				name: file.name || 'image',
				previewUrl: URL.createObjectURL(file),
				state: 'uploading',
				attachment: null,
				problem: null,
				detail: ''
			});
			void this.#upload(id);
		}
	}

	retry(id: string) {
		void this.#upload(id);
	}

	remove(id: string) {
		this.#uploads.get(id)?.abort();
		this.#uploads.delete(id);
		this.#files.delete(id);
		const item = this.items.find((candidate) => candidate.id === id);
		if (item) URL.revokeObjectURL(item.previewUrl);
		this.items = this.items.filter((candidate) => candidate.id !== id);
		this.notice = null;
	}

	/** Lets go of every image, for example after they were sent. */
	clear() {
		for (const controller of this.#uploads.values()) controller.abort();
		for (const item of this.items) URL.revokeObjectURL(item.previewUrl);
		this.#uploads.clear();
		this.#files.clear();
		this.items = [];
		this.notice = null;
	}

	async #upload(id: string) {
		const file = this.#files.get(id);
		const item = this.items.find((candidate) => candidate.id === id);
		if (!file || !item) return;
		const controller = new AbortController();
		this.#uploads.set(id, controller);
		item.state = 'uploading';
		item.problem = null;
		item.detail = '';
		try {
			const prepared = await prepareImage(file);
			item.attachment = await uploadImage(prepared, controller.signal);
			item.state = 'ready';
		} catch (error) {
			if (controller.signal.aborted) return;
			item.state = 'failed';
			item.problem = error instanceof ImageRejected ? error.problem : 'upload';
			item.detail = error instanceof ImageRejected ? error.detail : '';
		} finally {
			if (this.#uploads.get(id) === controller) this.#uploads.delete(id);
		}
	}
}
