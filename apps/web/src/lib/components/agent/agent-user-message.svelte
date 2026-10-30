<script lang="ts">
	import { attachmentUrl } from '$lib/agent/images';
	import type { AgentAttachment } from '$lib/agent/types';
	import { m } from '$lib/paraglide/messages';

	interface Props {
		text: string;
		images: AgentAttachment[];
		onOpenImage: (image: AgentAttachment) => void;
	}

	let { text, images, onOpenImage }: Props = $props();
</script>

<div class="flex flex-col items-end gap-2" data-slot="agent-user-message">
	{#if images.length > 0}
		<ul class="flex max-w-[85%] flex-wrap justify-end gap-2">
			{#each images as image (image.id)}
				<li>
					<button
						type="button"
						class="block overflow-hidden rounded-lg border bg-muted focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
						aria-label={m.agent_open_image({ name: image.filename })}
						onclick={() => onOpenImage(image)}
					>
						<img
							src={attachmentUrl(image.id)}
							alt=""
							width={image.width}
							height={image.height}
							loading="lazy"
							class="h-24 w-auto max-w-56 object-cover"
						/>
					</button>
				</li>
			{/each}
		</ul>
	{/if}
	{#if text !== ''}
		<p
			class="max-w-[85%] rounded-2xl bg-muted px-3.5 py-2 text-sm leading-6 break-words whitespace-pre-wrap"
		>
			{text}
		</p>
	{/if}
</div>
