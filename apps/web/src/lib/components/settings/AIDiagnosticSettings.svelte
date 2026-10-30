<script lang="ts">
	import { Button } from '$lib/components/ui/button';
	import InlineNotice from '$lib/components/inline-notice.svelte';
	import SectionHeader from '$lib/components/section-header.svelte';
	import { client } from '$lib/api/client';
	import { m } from '$lib/paraglide/messages';

	type DiagnosticResult = {
		provider: string;
		model: string;
		valid: boolean;
		code: string;
		failure?: string;
		latency_ms: number;
	};

	let running = $state(false);
	let result = $state<DiagnosticResult | null>(null);
	let error = $state('');

	const failure = $derived(
		result && !result.valid ? diagnosticFailure(result.code, result.failure) : ''
	);

	function diagnosticFailure(code: string, fallback?: string) {
		switch (code) {
			case 'not_configured':
				return m.settings_ai_diagnostic_not_configured();
			case 'invalid_credentials':
				return m.settings_ai_diagnostic_invalid_credentials();
			case 'rate_limited':
				return m.settings_ai_diagnostic_rate_limited();
			case 'timeout':
				return m.settings_ai_diagnostic_timeout();
			case 'empty_response':
				return m.settings_ai_diagnostic_empty_response();
			case 'malformed_response':
				return m.settings_ai_diagnostic_malformed_response();
			case 'provider_error':
				return m.settings_ai_diagnostic_provider_error();
			default:
				return fallback || m.settings_ai_diagnostic_provider_error();
		}
	}

	async function runDiagnostic() {
		running = true;
		error = '';
		try {
			const response = await client.POST('/admin/ai-diagnostic');
			if (response.error) {
				result = null;
				error = m.settings_ai_diagnostic_failed();
				return;
			}
			result = response.data ?? null;
		} catch {
			result = null;
			error = m.settings_ai_diagnostic_failed();
		} finally {
			running = false;
		}
	}
</script>

<div data-testid="ai-diagnostic">
	<SectionHeader
		title={m.settings_ai_diagnostic_title()}
		description={m.settings_ai_diagnostic_body()}
		themeIconRole="settings"
		class="mb-4"
	/>

	<div class="flex flex-wrap items-center gap-3">
		<Button variant="outline" size="sm" disabled={running} onclick={runDiagnostic}>
			{running ? m.settings_ai_diagnostic_running() : m.settings_ai_diagnostic_run()}
		</Button>
	</div>

	<div class="mt-4 space-y-3" aria-live="polite">
		{#if error}
			<InlineNotice tone="error" message={error} />
		{/if}
		{#if result}
			<InlineNotice
				tone={result.valid ? 'success' : 'error'}
				message={result.valid ? m.settings_ai_diagnostic_success() : failure}
			/>
			<dl class="grid gap-4 rounded-lg border bg-muted/20 p-4 sm:grid-cols-3">
				<div class="min-w-0">
					<dt class="text-sm text-muted-foreground">
						{m.settings_ai_diagnostic_provider()}
					</dt>
					<dd class="mt-1 truncate font-medium" title={result.provider}>
						{result.provider || '—'}
					</dd>
				</div>
				<div class="min-w-0">
					<dt class="text-sm text-muted-foreground">
						{m.settings_ai_diagnostic_model()}
					</dt>
					<dd class="mt-1 truncate font-mono text-sm" title={result.model}>
						{result.model || '—'}
					</dd>
				</div>
				<div class="min-w-0">
					<dt class="text-sm text-muted-foreground">
						{m.settings_ai_diagnostic_latency()}
					</dt>
					<dd class="mt-1 font-medium">{result.latency_ms} ms</dd>
				</div>
			</dl>
		{/if}
	</div>
</div>
