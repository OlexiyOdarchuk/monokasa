<script lang="ts">
	import { api, type Show, ApiError } from '$lib/api';

	let shows = $state<Show[]>([]);
	let loaded = $state(false);
	let error = $state('');

	$effect(() => {
		(async () => {
			try {
				shows = await api.get<Show[]>('/api/admin/shows');
			} catch (e) {
				if (e instanceof ApiError) error = e.detail || e.code;
				else error = String(e);
			} finally {
				loaded = true;
			}
		})();
	});

	function formatDate(iso: string): string {
		const d = new Date(iso);
		return d.toLocaleString('uk-UA', {
			day: 'numeric',
			month: 'long',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}
</script>

<svelte:head>
	<title>monokasa · події</title>
</svelte:head>

<div class="flex items-center justify-between gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">Події</h1>
	<div class="flex items-center gap-2">
		<a
			href="/admin/organizer"
			class="rounded-md border border-neutral-700 px-3 py-2 text-sm text-neutral-300 hover:bg-neutral-800"
		>
			🪪 Профіль
		</a>
		<a
			href="/admin/audit"
			class="rounded-md border border-neutral-700 px-3 py-2 text-sm text-neutral-300 hover:bg-neutral-800"
		>
			📜 Журнал
		</a>
		<a
			href="/admin/shows/new"
			class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)]"
		>
			+ Створити подію
		</a>
	</div>
</div>

{#if error}
	<div class="mt-6 rounded-md border border-red-900 bg-red-950/50 p-4 text-sm text-red-300">
		{error}
	</div>
{:else if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else if shows.length === 0}
	<div class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center">
		<p class="text-neutral-400">Ще немає жодної події.</p>
		<a
			href="/admin/shows/new"
			class="mt-3 inline-block text-sm text-[var(--color-brand)] hover:underline"
		>
			Створити першу →
		</a>
	</div>
{:else}
	<ul class="mt-6 grid gap-3">
		{#each shows as show (show.id)}
			<li>
				<a
					href="/admin/shows/{show.id}"
					class="block rounded-lg border bg-neutral-900 p-4 hover:bg-neutral-800/70 {show.archived_at
						? 'border-neutral-800 opacity-60'
						: 'border-neutral-800'}"
				>
					<div class="flex items-baseline justify-between gap-3">
						<h2 class="text-lg font-medium text-neutral-100">
							{show.title}
							{#if show.archived_at}
								<span class="ml-2 rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-400">в архіві</span>
							{/if}
						</h2>
						<span class="shrink-0 text-sm text-neutral-500">{formatDate(show.starts_at)}</span>
					</div>
					{#if show.venue}
						<p class="mt-1 text-sm text-neutral-400">{show.venue}</p>
					{/if}
				</a>
			</li>
		{/each}
	</ul>
{/if}
