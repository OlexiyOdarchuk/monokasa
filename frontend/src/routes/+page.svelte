<script lang="ts">
	import { publicApi, type PublicShowSummary, ApiError } from '$lib/api';

	let shows = $state<PublicShowSummary[]>([]);
	let loaded = $state(false);
	let error = $state('');

	$effect(() => {
		(async () => {
			try {
				shows = await publicApi.get<PublicShowSummary[]>('/api/public/shows');
			} catch (e) {
				if (e instanceof ApiError) error = e.detail || e.code;
				else error = String(e);
			} finally {
				loaded = true;
			}
		})();
	});

	function formatDateTime(iso: string): string {
		return new Date(iso).toLocaleString('uk-UA', {
			weekday: 'short',
			day: 'numeric',
			month: 'long',
			hour: '2-digit',
			minute: '2-digit'
		});
	}
</script>

<svelte:head>
	<title>monokasa — квитки на події</title>
</svelte:head>

<main class="mx-auto max-w-3xl p-4 sm:p-6">
	<header class="mt-4 mb-8 text-center sm:mt-12 sm:mb-12">
		<h1 class="text-4xl font-bold tracking-tight sm:text-5xl">monokasa</h1>
		<p class="mt-2 text-neutral-400">Квитки на події через monobank</p>
	</header>

	{#if !loaded}
		<div class="text-center text-neutral-500">Завантажую афішу…</div>
	{:else if error}
		<div class="rounded-lg border border-red-900 bg-red-950/50 p-4 text-center text-sm text-red-300">
			{error}
		</div>
	{:else if shows.length === 0}
		<div class="rounded-2xl border border-neutral-800 bg-neutral-900 p-8 text-center">
			<div class="text-3xl">🎭</div>
			<h2 class="mt-3 text-lg font-medium">Зараз подій немає</h2>
			<p class="mt-2 text-sm text-neutral-400">
				Загляни пізніше — нові події з'являться тут.
			</p>
		</div>
	{:else}
		<h2 class="mb-4 text-sm font-medium uppercase tracking-wider text-neutral-500">
			Афіша
		</h2>
		<ul class="grid gap-4 sm:grid-cols-2">
			{#each shows as show (show.slug)}
				{@const soldOut = show.seats_free === 0}
				<li>
					<a
						href="/event/{show.slug}"
						class="group block h-full overflow-hidden rounded-xl border border-neutral-800 bg-neutral-900 transition hover:border-neutral-700"
						class:opacity-60={soldOut}
					>
						<!-- Poster header: real image if set, otherwise a deterministic gradient
						     keyed off the slug so each card stays visually distinct. -->
						{#if show.poster_url}
							<div class="aspect-[16/9] overflow-hidden bg-neutral-950">
								<img
									src={show.poster_url}
									alt={show.title}
									class="h-full w-full object-cover transition group-hover:scale-[1.02]"
									loading="lazy"
									onerror={(e: Event) => ((e.target as HTMLImageElement).style.display = 'none')}
								/>
							</div>
						{:else}
							<div class="aspect-[16/9] bg-gradient-to-br from-neutral-800 via-neutral-900 to-black"></div>
						{/if}

						<div class="p-5">
							<div class="flex items-baseline justify-between gap-3">
								<h3 class="text-lg font-semibold text-neutral-100">{show.title}</h3>
								{#if soldOut}
									<span class="shrink-0 rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-400">
										sold out
									</span>
								{/if}
							</div>
							<p class="mt-1 text-sm text-[var(--color-brand)]">
								{formatDateTime(show.starts_at)}
							</p>
							{#if show.venue}
								<p class="mt-1 text-sm text-neutral-400">📍 {show.venue}</p>
							{/if}
							{#if show.description}
								<p class="mt-2 line-clamp-2 text-sm text-neutral-400">
									{show.description}
								</p>
							{/if}
							{#if !soldOut}
								{@const lowStock = show.seats_free <= 5}
								<p class="mt-3 text-xs {lowStock ? 'font-medium text-amber-400' : 'text-neutral-500'}">
									{#if lowStock}
										🔥 Залишилось {show.seats_free} з {show.seats_total}
									{:else}
										{show.seats_free} / {show.seats_total} місць вільно
									{/if}
								</p>
							{/if}
						</div>
					</a>
				</li>
			{/each}
		</ul>
	{/if}

	<footer class="mt-16 border-t border-neutral-900 pt-6 pb-8 text-center text-xs text-neutral-600">
		<a href="/admin/login" class="hover:text-neutral-400">Вхід для адміна</a>
	</footer>
</main>
