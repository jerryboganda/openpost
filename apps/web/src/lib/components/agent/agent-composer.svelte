<script lang="ts">
	import type { AttachmentTray, PendingImage } from '$lib/agent/attachments.svelte';
	import { IMAGE_ACCEPT, MAX_IMAGES, isAcceptedImage } from '$lib/agent/image-policy';
	import { EFFORTS } from '$lib/agent/preferences';
	import type { AgentSession } from '$lib/agent/session.svelte';
	import type { AgentAccess, AgentEffort } from '$lib/agent/types';
	import InlineNotice from '$lib/components/inline-notice.svelte';
	import { Button } from '$lib/components/ui/button';
	import * as Select from '$lib/components/ui/select';
	import { Textarea } from '$lib/components/ui/textarea';
	import { m } from '$lib/paraglide/messages';
	import { ProtectedIcon, ThemeIcon } from '$lib/themes/icons';
	import { cn } from '$lib/utils';

	interface Props {
		session: AgentSession;
		tray: AttachmentTray;
		value: string;
		textarea: HTMLTextAreaElement | null;
		/** Why the agent cannot take messages, or empty when it can. */
		unavailableReason: string;
		onSend: () => void;
	}

	let {
		session,
		tray,
		value = $bindable(''),
		textarea = $bindable(null),
		unavailableReason,
		onSend
	}: Props = $props();

	let picker = $state<HTMLInputElement | null>(null);

	const blocked = $derived(unavailableReason !== '');
	const canSend = $derived(
		!blocked &&
			!session.working &&
			!tray.busy &&
			!tray.failed &&
			(value.trim() !== '' || tray.ready.length > 0)
	);
	const choosableEngines = $derived(session.active === null ? session.engines : []);
	const engineLabel = $derived(
		session.engines.find((engine) => engine.id === session.engine)?.label ?? session.engine
	);
	const accessOptions: AgentAccess[] = ['full', 'read_only'];

	function effortLabel(effort: AgentEffort): string {
		switch (effort) {
			case 'low':
				return m.agent_effort_low();
			case 'medium':
				return m.agent_effort_medium();
			case 'high':
				return m.agent_effort_high();
			default:
				return m.agent_effort_auto();
		}
	}

	function accessLabel(access: AgentAccess): string {
		return access === 'read_only' ? m.agent_access_read_only() : m.agent_access_full();
	}

	function chooseEffort(value: string) {
		const effort = EFFORTS.find((candidate) => candidate === value);
		if (effort) session.setEffort(effort);
	}

	function chooseAccess(value: string) {
		const access = accessOptions.find((candidate) => candidate === value);
		if (access) session.setAccess(access);
	}

	function imageProblem(item: PendingImage): string {
		switch (item.problem) {
			case 'too_large':
				return m.agent_image_error_large({ name: item.name });
			case 'unreadable':
				return m.agent_image_error_unreadable({ name: item.name });
			default:
				return item.detail || m.agent_image_error_upload({ name: item.name });
		}
	}

	function onKeydown(event: KeyboardEvent) {
		if (event.key !== 'Enter' || event.isComposing) return;
		const touch = globalThis.matchMedia?.('(pointer: coarse)').matches ?? false;
		const submits = event.metaKey || event.ctrlKey || (!event.shiftKey && !touch);
		if (!submits) return;
		event.preventDefault();
		if (canSend) onSend();
	}

	function onPaste(event: ClipboardEvent) {
		const files = Array.from(event.clipboardData?.files ?? []).filter((file) =>
			isAcceptedImage(file.type)
		);
		if (files.length === 0) return;
		// Pasted screenshots carry no text. When text comes along, it still pastes.
		if (!event.clipboardData?.getData('text/plain')) event.preventDefault();
		tray.add(files);
	}

	function onPick(event: Event & { currentTarget: HTMLInputElement }) {
		tray.add(Array.from(event.currentTarget.files ?? []));
		event.currentTarget.value = '';
	}

	const triggerClass =
		'min-w-0 max-w-44 border-transparent bg-transparent text-muted-foreground hover:bg-muted hover:text-foreground';
</script>

<form
	class="shrink-0 px-3 pb-3 sm:px-4 sm:pb-4"
	onsubmit={(event) => {
		event.preventDefault();
		if (canSend) onSend();
	}}
>
	<div class="mx-auto flex w-full max-w-3xl flex-col gap-2">
		{#if blocked}
			<InlineNotice tone="warning" size="compact" message={unavailableReason} />
		{/if}
		{#if tray.notice === 'type'}
			<InlineNotice tone="warning" size="compact" message={m.agent_image_error_type()} />
		{:else if tray.notice === 'limit'}
			<InlineNotice
				tone="warning"
				size="compact"
				message={m.agent_image_error_limit({ count: MAX_IMAGES })}
			/>
		{/if}
		<div
			class="rounded-xl border bg-background shadow-xs focus-within:border-ring focus-within:ring-2 focus-within:ring-ring/30"
		>
			{#if tray.items.length > 0}
				<ul class="flex flex-wrap gap-3 px-3 pt-3">
					{#each tray.items as item (item.id)}
						<li class="relative">
							<img
								src={item.previewUrl}
								alt={item.name}
								class={cn(
									'size-16 rounded-lg border object-cover',
									item.state !== 'ready' && 'opacity-60'
								)}
							/>
							{#if item.state === 'uploading'}
								<span
									class="absolute inset-0 flex items-center justify-center"
									role="status"
									aria-label={m.agent_image_uploading({ name: item.name })}
								>
									<ProtectedIcon
										icon="loading"
										class="size-5 animate-spin motion-reduce:animate-none"
									/>
								</span>
							{:else if item.state === 'failed'}
								<button
									type="button"
									class="absolute inset-0 flex flex-col items-center justify-center gap-0.5 rounded-lg bg-background/70 text-xs text-action-destructive-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
									aria-label={m.agent_image_retry({ name: item.name })}
									onclick={() => tray.retry(item.id)}
								>
									<ProtectedIcon icon="error" class="size-4" />
									{m.agent_image_retry_short()}
								</button>
							{/if}
							<Button
								variant="outline"
								size="icon-xs"
								class="absolute -top-2 -right-2 rounded-full bg-background"
								aria-label={m.agent_remove_image({ name: item.name })}
								onclick={() => tray.remove(item.id)}
							>
								<ThemeIcon role="close" />
							</Button>
						</li>
					{/each}
				</ul>
				{#each tray.items.filter((item) => item.state === 'failed') as item (item.id)}
					<p class="px-3 pt-2 text-xs text-action-destructive-foreground" role="alert">
						{imageProblem(item)}
					</p>
				{/each}
			{/if}
			<Textarea
				unstyled
				bind:ref={textarea}
				bind:value
				rows={1}
				placeholder={m.agent_composer_placeholder()}
				aria-label={m.agent_composer_label()}
				disabled={blocked}
				class="field-sizing-content max-h-52 min-h-11 w-full resize-none bg-transparent px-3.5 py-3 text-base outline-none placeholder:text-muted-foreground disabled:cursor-not-allowed md:text-sm"
				onkeydown={onKeydown}
				onpaste={onPaste}
			/>
			<div class="flex items-end justify-between gap-2 p-2">
				<div class="flex min-w-0 flex-1 flex-wrap items-center gap-1">
					<input
						bind:this={picker}
						type="file"
						accept={IMAGE_ACCEPT}
						multiple
						class="sr-only"
						tabindex="-1"
						aria-hidden="true"
						onchange={onPick}
					/>
					<Button
						variant="ghost"
						size="icon-sm"
						aria-label={m.agent_attach()}
						title={m.agent_attach()}
						disabled={blocked || tray.full}
						onclick={() => picker?.click()}
					>
						<ThemeIcon role="image-add" />
					</Button>
					{#if choosableEngines.length > 1}
						<Select.Root
							type="single"
							value={session.engine}
							onValueChange={(next) => session.setEngine(next)}
						>
							<Select.Trigger size="sm" class={triggerClass} aria-label={m.agent_engine()}>
								<span class="truncate">{engineLabel}</span>
							</Select.Trigger>
							<Select.Content>
								{#each choosableEngines as engine (engine.id)}
									<Select.Item value={engine.id} label={engine.label} disabled={!engine.available}>
										<div class="flex min-w-0 flex-col">
											<span class="truncate">{engine.label}</span>
											{#if !engine.available && engine.reason}
												<span class="text-xs font-normal text-muted-foreground">{engine.reason}</span>
											{/if}
										</div>
									</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
					{/if}
					{#if session.builtIn}
						<Select.Root
							type="single"
							value={session.preferences.model}
							onValueChange={(next) => session.setModel(next)}
						>
							<Select.Trigger size="sm" class={triggerClass} aria-label={m.agent_model()}>
								<span class="truncate"
									>{session.selectedModel?.label ?? session.preferences.model}</span
								>
							</Select.Trigger>
							<Select.Content>
								{#each session.models as option (option.id)}
									<Select.Item value={option.id} label={option.label}>
										<div class="flex min-w-0 flex-col">
											<span class="truncate"
												>{option.vision
													? option.label
													: m.agent_model_text_only({ model: option.label })}</span
											>
											<span class="text-xs font-normal text-muted-foreground"
												>{option.description}</span
											>
										</div>
									</Select.Item>
								{/each}
							</Select.Content>
						</Select.Root>
						<Select.Root
							type="single"
							value={session.preferences.effort}
							onValueChange={chooseEffort}
						>
							<Select.Trigger size="sm" class={triggerClass} aria-label={m.agent_effort()}>
								<span class="truncate"
									>{m.agent_effort_value({ level: effortLabel(session.preferences.effort) })}</span
								>
							</Select.Trigger>
							<Select.Content>
								{#each EFFORTS as effort (effort)}
									<Select.Item value={effort} label={effortLabel(effort)} />
								{/each}
							</Select.Content>
						</Select.Root>
						<Select.Root
							type="single"
							value={session.preferences.access}
							onValueChange={chooseAccess}
						>
							<Select.Trigger size="sm" class={triggerClass} aria-label={m.agent_access()}>
								<span class="truncate">{accessLabel(session.preferences.access)}</span>
							</Select.Trigger>
							<Select.Content>
								{#each accessOptions as access (access)}
									<Select.Item value={access} label={accessLabel(access)} />
								{/each}
							</Select.Content>
						</Select.Root>
					{/if}
				</div>
				{#if session.working}
					<Button
						variant="outline"
						size="icon"
						class="shrink-0 rounded-full"
						aria-label={m.agent_stop()}
						title={m.agent_stop()}
						disabled={session.activeRun === null}
						onclick={() => void session.stop()}
					>
						<ProtectedIcon icon="editor-stop" class="size-4" />
					</Button>
				{:else}
					<Button
						type="submit"
						size="icon"
						class="shrink-0 rounded-full"
						aria-label={m.agent_send()}
						title={m.agent_send()}
						disabled={!canSend}
					>
						<ThemeIcon role="arrow-up" />
					</Button>
				{/if}
			</div>
		</div>
		<p class="hidden text-center text-xs text-muted-foreground [@media(pointer:fine)]:block">
			{m.agent_keyboard_hint()}
		</p>
	</div>
</form>
