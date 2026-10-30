<script lang="ts">
	import { parseMarkdown, type BlockNode, type InlineNode } from '$lib/agent/markdown';
	import AgentCodeBlock from './agent-code-block.svelte';

	interface Props {
		source: string;
	}

	let { source }: Props = $props();

	const blocks = $derived(parseMarkdown(source));

	function headingTag(level: number): string {
		return `h${Math.min(level + 2, 6)}`;
	}

	function headingClass(level: number): string {
		return level <= 2 ? 'mt-2 text-base font-semibold' : 'mt-1 text-sm font-semibold';
	}

	function opensElsewhere(href: string): boolean {
		return href.startsWith('http://') || href.startsWith('https://');
	}
</script>

{#snippet inline(nodes: InlineNode[])}
	{#each nodes as node, index (index)}
		{#if node.type === 'text'}{node.text}{:else if node.type === 'code'}<code
				class="rounded bg-muted px-1 py-0.5 text-[0.85em] break-words">{node.text}</code
			>{:else if node.type === 'strong'}<strong class="font-semibold"
				>{@render inline(node.children)}</strong
			>{:else if node.type === 'em'}<em>{@render inline(node.children)}</em
			>{:else if node.type === 'del'}<del>{@render inline(node.children)}</del
			>{:else if node.type === 'link'}<a
				href={node.href}
				class="text-action-link underline underline-offset-2 hover:text-action-link-hover"
				target={opensElsewhere(node.href) ? '_blank' : undefined}
				rel={opensElsewhere(node.href) ? 'noopener noreferrer' : undefined}
				>{@render inline(node.children)}</a
			>{:else}<br />{/if}
	{/each}
{/snippet}

{#snippet render(nodes: BlockNode[])}
	{#each nodes as block, index (index)}
		{#if block.type === 'paragraph'}
			<p>{@render inline(block.children)}</p>
		{:else if block.type === 'heading'}
			<svelte:element this={headingTag(block.level)} class={headingClass(block.level)}>
				{@render inline(block.children)}
			</svelte:element>
		{:else if block.type === 'code'}
			<AgentCodeBlock code={block.text} lang={block.lang} streaming={!block.closed} />
		{:else if block.type === 'list'}
			{#if block.ordered}
				<ol start={block.start} class="list-decimal space-y-1 pl-5">
					{#each block.items as item, itemIndex (itemIndex)}
						<li class="[&>ol]:mt-1 [&>p+*]:mt-1 [&>ul]:mt-1">{@render render(item)}</li>
					{/each}
				</ol>
			{:else}
				<ul class="list-disc space-y-1 pl-5">
					{#each block.items as item, itemIndex (itemIndex)}
						<li class="[&>ol]:mt-1 [&>p+*]:mt-1 [&>ul]:mt-1">{@render render(item)}</li>
					{/each}
				</ul>
			{/if}
		{:else if block.type === 'quote'}
			<blockquote class="flex flex-col gap-2 border-l-2 pl-3 text-muted-foreground">
				{@render render(block.children)}
			</blockquote>
		{:else if block.type === 'table'}
			<div class="overflow-x-auto rounded-lg border">
				<table class="w-full border-collapse text-left text-sm">
					<thead class="bg-muted/50">
						<tr>
							{#each block.head as cell, column (column)}
								<th
									scope="col"
									class="border-b px-3 py-1.5 font-semibold"
									style:text-align={block.align[column] ?? undefined}
									>{@render inline(cell)}</th
								>
							{/each}
						</tr>
					</thead>
					<tbody>
						{#each block.rows as row, rowIndex (rowIndex)}
							<tr class="border-b last:border-b-0">
								{#each row as cell, column (column)}
									<td
										class="px-3 py-1.5 align-top"
										style:text-align={block.align[column] ?? undefined}
										>{@render inline(cell)}</td
									>
								{/each}
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{:else}
			<hr class="border-border" />
		{/if}
	{/each}
{/snippet}

<div class="flex min-w-0 flex-col gap-3 text-sm leading-6 break-words" data-slot="agent-markdown">
	{@render render(blocks)}
</div>
