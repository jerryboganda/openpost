<script lang="ts">
	import type { ThemeIconRole } from '$lib/themes';
	import { m } from '$lib/paraglide/messages';
	import { ThemeIcon } from '$lib/themes/icons';

	interface Props {
		/** Puts a starter prompt in the composer. Nothing is sent. */
		onPick: (prompt: string) => void;
	}

	interface Suggestion {
		icon: ThemeIconRole;
		label: string;
		prompt: string;
	}

	let { onPick }: Props = $props();

	const suggestions = $derived<Suggestion[]>([
		{
			icon: 'analytics',
			label: m.agent_suggestion_health(),
			prompt: m.agent_suggestion_health_prompt()
		},
		{
			icon: 'launch',
			label: m.agent_suggestion_deploy(),
			prompt: m.agent_suggestion_deploy_prompt()
		},
		{
			icon: 'search',
			label: m.agent_suggestion_bug(),
			prompt: m.agent_suggestion_bug_prompt()
		},
		{
			icon: 'code',
			label: m.agent_suggestion_explain(),
			prompt: m.agent_suggestion_explain_prompt()
		}
	]);
</script>

<div class="flex flex-col items-center gap-6 py-8 text-center sm:py-14" data-slot="agent-empty">
	<span class="flex size-11 items-center justify-center rounded-full bg-muted">
		<ThemeIcon role="assistant" class="size-5" />
	</span>
	<div class="flex max-w-md flex-col gap-1.5">
		<h2 class="text-xl font-semibold">{m.agent_empty_title()}</h2>
		<p class="text-sm text-muted-foreground">{m.agent_empty_description()}</p>
	</div>
	<ul class="grid w-full max-w-xl gap-2 text-left sm:grid-cols-2">
		{#each suggestions as suggestion (suggestion.label)}
			<li>
				<button
					type="button"
					class="flex min-h-11 w-full items-start gap-3 rounded-lg border px-3 py-2.5 text-sm hover:bg-muted/50 focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
					onclick={() => onPick(suggestion.prompt)}
				>
					<ThemeIcon role={suggestion.icon} class="mt-0.5 size-4 shrink-0 text-muted-foreground" />
					<span>{suggestion.label}</span>
				</button>
			</li>
		{/each}
	</ul>
</div>
