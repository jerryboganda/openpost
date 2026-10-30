<script lang="ts">
	import { attachmentUrl } from '$lib/agent/images';
	import type { AgentAttachment } from '$lib/agent/types';
	import * as Dialog from '$lib/components/ui/dialog';

	interface Props {
		image: AgentAttachment | null;
	}

	let { image = $bindable(null) }: Props = $props();
</script>

<Dialog.Root
	open={image !== null}
	onOpenChange={(open) => {
		if (!open) image = null;
	}}
>
	<Dialog.Content class="sm:max-w-4xl">
		<Dialog.Header>
			<Dialog.Title class="truncate pr-8">{image?.filename ?? ''}</Dialog.Title>
			<Dialog.Description>{image ? `${image.width} × ${image.height}` : ''}</Dialog.Description>
		</Dialog.Header>
		{#if image}
			<img
				src={attachmentUrl(image.id)}
				alt={image.filename}
				class="mx-auto max-h-[75dvh] w-auto max-w-full rounded-md"
			/>
		{/if}
	</Dialog.Content>
</Dialog.Root>
