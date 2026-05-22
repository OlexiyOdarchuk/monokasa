<script lang="ts">
	import { publicApi, type Organizer, ApiError } from '$lib/api';
	import { safeHref, safeImageSrc } from '$lib/safe';

	let o = $state<Organizer | null>(null);
	let loaded = $state(false);
	let error = $state('');

	$effect(() => {
		(async () => {
			try {
				o = await publicApi.get<Organizer>('/api/public/organizer');
			} catch (e) {
				if (e instanceof ApiError) error = e.detail || e.code;
				else error = String(e);
			} finally {
				loaded = true;
			}
		})();
	});

	const hasAny = $derived(
		!!o &&
			(o.name ||
				o.bio ||
				o.contact_email ||
				o.phone ||
				o.website_url ||
				o.telegram_url ||
				o.instagram_url ||
				o.facebook_url ||
				o.logo_url)
	);

</script>

<svelte:head>
	<title>Про організатора — monokasa</title>
	<meta name="description" content={o?.bio || 'Про організатора'} />
</svelte:head>

<main class="mx-auto max-w-2xl p-4 sm:p-6 pb-16">
	<div class="flex items-center gap-3">
		<a href="/" class="text-sm text-neutral-400 hover:text-neutral-200">← До подій</a>
	</div>

	{#if !loaded}
		<div class="mt-10 text-center text-neutral-500">Завантажую…</div>
	{:else if error}
		<div class="mt-6 rounded-md border border-red-900 bg-red-950/50 p-4 text-sm text-red-300">
			{error}
		</div>
	{:else if !hasAny}
		<div class="mt-10 rounded-2xl border border-neutral-800 bg-neutral-900 p-8 text-center">
			<div class="text-4xl">🪪</div>
			<h1 class="mt-3 text-xl font-semibold">Профіль ще не налаштований</h1>
			<p class="mt-2 text-sm text-neutral-400">
				Адмін цього інстансу ще не заповнив свій профіль організатора. Поки що тут нічого.
			</p>
		</div>
	{:else if o}
		<section class="mt-6">
			{#if o.logo_url}
				<img
					src={safeImageSrc(o.logo_url)}
					alt={o.name || 'Логотип'}
					class="mb-5 h-24 w-24 rounded-xl border border-neutral-800 object-cover"
					onerror={(e: Event) => ((e.target as HTMLImageElement).style.display = 'none')}
				/>
			{/if}

			{#if o.name}
				<h1 class="text-3xl font-semibold tracking-tight">{o.name}</h1>
			{:else}
				<h1 class="text-3xl font-semibold tracking-tight text-neutral-400">Про організатора</h1>
			{/if}

			{#if o.bio}
				<p class="mt-4 whitespace-pre-line text-base text-neutral-300">{o.bio}</p>
			{/if}

			{#if o.contact_email || o.phone}
				<div class="mt-6 flex flex-col gap-1 text-sm">
					{#if o.contact_email}
						<a
							href="mailto:{o.contact_email}"
							class="text-neutral-300 hover:text-white"
						>
							✉️ {o.contact_email}
						</a>
					{/if}
					{#if o.phone}
						<a href="tel:{o.phone}" class="text-neutral-300 hover:text-white">
							📞 {o.phone}
						</a>
					{/if}
				</div>
			{/if}

			{#if o.website_url || o.telegram_url || o.instagram_url || o.facebook_url}
				<div class="mt-6 flex flex-wrap gap-2">
					{#if o.website_url}
						<a
							href={safeHref(o.website_url)}
							target="_blank"
							rel="noopener"
							class="rounded-md border border-neutral-700 px-3 py-1.5 text-sm hover:bg-neutral-800"
						>
							🌐 Сайт
						</a>
					{/if}
					{#if o.telegram_url}
						<a
							href={safeHref(o.telegram_url)}
							target="_blank"
							rel="noopener"
							class="rounded-md border border-sky-700 bg-sky-950/30 px-3 py-1.5 text-sm text-sky-200 hover:bg-sky-950/60"
						>
							💬 Telegram
						</a>
					{/if}
					{#if o.instagram_url}
						<a
							href={safeHref(o.instagram_url)}
							target="_blank"
							rel="noopener"
							class="rounded-md border border-pink-700 bg-pink-950/20 px-3 py-1.5 text-sm text-pink-200 hover:bg-pink-950/50"
						>
							📷 Instagram
						</a>
					{/if}
					{#if o.facebook_url}
						<a
							href={safeHref(o.facebook_url)}
							target="_blank"
							rel="noopener"
							class="rounded-md border border-blue-700 bg-blue-950/20 px-3 py-1.5 text-sm text-blue-200 hover:bg-blue-950/50"
						>
							👥 Facebook
						</a>
					{/if}
				</div>
			{/if}
		</section>
	{/if}
</main>
