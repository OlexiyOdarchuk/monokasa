<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, type Show, type UpdateShowInput, type SeatCategory, ApiError } from '$lib/api';
	import DateTimePicker from '$lib/DateTimePicker.svelte';

	const id = $derived(Number(page.params.id));

	let show = $state<Show | null>(null);
	let loaded = $state(false);
	let error = $state('');

	// Pricing tiers (categories). Loaded alongside the show; admin
	// edits them inline and each Save hits POST /categories which
	// upserts and batch-updates every seat with the matching name.
	let categories = $state<SeatCategory[]>([]);
	let catName = $state('');
	let catColor = $state('#3b82f6');
	let catPrice = $state(250);
	let catSaving = $state(false);
	let catError = $state('');

	// Edit form state (mirrors show fields; populated when show loads).
	let editTitle = $state('');
	let editVenue = $state('');
	let editStartsAt = $state(''); // RFC3339 UTC, two-way bound to DateTimePicker
	let editDescription = $state('');
	let editPosterURL = $state('');
	let editSessionGroup = $state('');
	let uploading = $state(false);
	let uploadError = $state('');

	async function uploadPoster(file: File | undefined) {
		if (!file) return;
		uploadError = '';
		uploading = true;
		try {
			const form = new FormData();
			form.append('file', file);
			const r = await fetch('/api/admin/posters', {
				method: 'POST',
				credentials: 'same-origin',
				body: form
			});
			if (r.status === 401) {
				window.location.href = '/admin/login';
				return;
			}
			if (!r.ok) {
				const err = (await r.json().catch(() => ({}))) as { detail?: string; error?: string };
				uploadError = err.detail || err.error || `HTTP ${r.status}`;
				return;
			}
			const { url } = (await r.json()) as { url: string };
			editPosterURL = url;
		} catch (e) {
			uploadError = String(e);
		} finally {
			uploading = false;
		}
	}
	let saving = $state(false);
	let savedAt = $state<Date | null>(null);

	let archiving = $state(false);

	// Copy event link to clipboard with a tiny "copied!" feedback.
	let copiedAt = $state<Date | null>(null);
	async function copyEventLink() {
		if (!show) return;
		const link = `${window.location.origin}/event/${show.slug}`;
		try {
			await navigator.clipboard.writeText(link);
			copiedAt = new Date();
			setTimeout(() => (copiedAt = null), 2000);
		} catch {
			// Fallback for browsers without clipboard API access (e.g. http).
			prompt('Скопіюй посилання вручну:', link);
		}
	}

	async function load() {
		try {
			show = await api.get<Show>(`/api/admin/shows/${id}`);
			editTitle = show.title;
			editVenue = show.venue;
			editStartsAt = show.starts_at;
			editDescription = show.description;
			editPosterURL = show.poster_url;
			editSessionGroup = show.session_group ?? '';
			categories = await api.get<SeatCategory[]>(`/api/admin/shows/${id}/categories`);
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			loaded = true;
		}
	}

	async function addCategory(e: Event) {
		e.preventDefault();
		if (!show) return;
		const name = catName.trim();
		if (!name) {
			catError = "Назва обов'язкова";
			return;
		}
		catSaving = true;
		catError = '';
		try {
			const c = await api.post<SeatCategory>(`/api/admin/shows/${id}/categories`, {
				name,
				color: catColor,
				price_kopecks: Math.round(catPrice * 100),
				sort_order: categories.length
			});
			// Upsert: replace if exists (by name), else append.
			const idx = categories.findIndex((x) => x.name === c.name);
			if (idx >= 0) categories[idx] = c;
			else categories = [...categories, c];
			catName = '';
		} catch (err) {
			catError = err instanceof ApiError ? err.detail || err.code : String(err);
		} finally {
			catSaving = false;
		}
	}

	async function updateCategoryPrice(c: SeatCategory, newPrice: number) {
		try {
			const updated = await api.post<SeatCategory>(`/api/admin/shows/${id}/categories`, {
				name: c.name,
				color: c.color,
				price_kopecks: Math.round(newPrice * 100),
				sort_order: c.sort_order
			});
			categories = categories.map((x) => (x.id === updated.id ? updated : x));
		} catch (err) {
			catError = err instanceof ApiError ? err.detail || err.code : String(err);
		}
	}

	async function removeCategory(c: SeatCategory) {
		if (!confirm(`Видалити категорію "${c.name}"?\n\nМісця залишать свій label, але втратять колір.`)) {
			return;
		}
		try {
			await api.del(`/api/admin/categories/${c.id}`);
			categories = categories.filter((x) => x.id !== c.id);
		} catch (err) {
			catError = err instanceof ApiError ? err.detail || err.code : String(err);
		}
	}

	$effect(() => {
		// Re-run on id change (back/forward navigation between shows).
		void id;
		load();
	});

	async function save(e: Event) {
		e.preventDefault();
		if (!show) return;
		saving = true;
		error = '';

		const patch: UpdateShowInput = {};
		if (editTitle !== show.title) patch.title = editTitle.trim();
		if (editVenue !== show.venue) patch.venue = editVenue.trim();
		if (editStartsAt !== show.starts_at) patch.starts_at = editStartsAt;
		if (editDescription !== show.description) patch.description = editDescription;
		if (editPosterURL !== show.poster_url) patch.poster_url = editPosterURL.trim();
		if (editSessionGroup !== (show.session_group ?? '')) patch.session_group = editSessionGroup.trim();

		if (Object.keys(patch).length === 0) {
			saving = false;
			savedAt = new Date(); // visually confirm "nothing to save"
			return;
		}

		try {
			show = await api.patch<Show>(`/api/admin/shows/${id}`, patch);
			editTitle = show.title;
			editVenue = show.venue;
			editStartsAt = show.starts_at;
			editDescription = show.description;
			editPosterURL = show.poster_url;
			savedAt = new Date();
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			saving = false;
		}
	}

	async function archive() {
		if (!confirm('Архівувати подію? Її не буде у дашборді, але дані лишаться.')) return;
		archiving = true;
		try {
			await api.post(`/api/admin/shows/${id}/archive`);
			await goto('/admin');
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
			archiving = false;
		}
	}

	function formatDate(iso: string): string {
		return new Date(iso).toLocaleString('uk-UA', {
			day: 'numeric',
			month: 'long',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	function formatUAH(kopecks: number): string {
		return (kopecks / 100).toLocaleString('uk-UA', { minimumFractionDigits: 2 }) + ' ₴';
	}
</script>

<svelte:head>
	<title>monokasa · {show?.title ?? 'подія'}</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← До списку</a>
</div>

{#if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else if !show}
	<div class="mt-6 rounded-md border border-red-900 bg-red-950/50 p-4 text-sm text-red-300">
		{error || 'Не вдалось завантажити подію'}
	</div>
{:else}
	<h1 class="mt-2 text-2xl font-semibold tracking-tight">
		{show.title}
		{#if show.archived_at}
			<span class="ml-2 rounded bg-neutral-800 px-2 py-1 align-middle text-xs text-neutral-400">
				в архіві з {formatDate(show.archived_at)}
			</span>
		{/if}
	</h1>

	<div class="mt-2 flex flex-wrap items-center gap-2 text-sm">
		<a
			href="/event/{show.slug}"
			target="_blank"
			rel="noopener"
			class="text-[var(--color-brand)] hover:underline"
		>
			📍 Сторінка події ↗
		</a>
		<button
			type="button"
			onclick={copyEventLink}
			class="rounded-md border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
		>
			{copiedAt ? '✓ скопійовано' : '📋 Copy link'}
		</button>
		<a
			href="/api/admin/shows/{show.id}/poster-qr.png"
			download="qr-{show.slug}.png"
			class="rounded-md border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
		>
			🔳 QR для афіші
		</a>
		<a
			href="/admin/shows/new?title={encodeURIComponent(show.title)}&venue={encodeURIComponent(show.venue)}&group={encodeURIComponent(show.session_group || show.slug)}"
			class="rounded-md border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-800"
			title="Створити ще одну дату цієї події"
		>
			🎭 Створити повтор
		</a>
	</div>

	{#if show.stats}
		<section class="mt-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
			<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
				<div class="text-xs text-neutral-500">Усього місць</div>
				<div class="mt-1 text-2xl font-semibold">{show.stats.total}</div>
			</div>
			<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
				<div class="text-xs text-neutral-500">Продано</div>
				<div class="mt-1 text-2xl font-semibold text-emerald-400">{show.stats.sold}</div>
			</div>
			<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
				<div class="text-xs text-neutral-500">В очікуванні</div>
				<div class="mt-1 text-2xl font-semibold text-amber-400">{show.stats.held}</div>
			</div>
			<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
				<div class="text-xs text-neutral-500">Вільно</div>
				<div class="mt-1 text-2xl font-semibold">{show.stats.free}</div>
			</div>
		</section>

		<div class="mt-4 rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			<div class="text-xs text-neutral-500">Виторг</div>
			<div class="mt-1 text-xl font-semibold">{formatUAH(show.stats.revenue_kopecks)}</div>
		</div>
	{/if}

	<section class="mt-6 grid gap-3 sm:grid-cols-2">
		{#if show.kind !== 'ga'}
			<a
				href="/admin/shows/{id}/layout"
				class="rounded-lg border border-neutral-800 bg-neutral-900 p-4 hover:bg-neutral-800/70"
			>
				<div class="font-medium">🎭 Редактор залу</div>
				<div class="mt-1 text-sm text-neutral-400">
					Drag&amp;drop місць, категорії, ціни
				</div>
			</a>
		{:else}
			<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4 opacity-70">
				<div class="font-medium">🎤 GA — без сидячих місць</div>
				<div class="mt-1 text-sm text-neutral-400">
					{show.ga_capacity} квитків у пулі. Покупець обирає кількість.
				</div>
			</div>
		{/if}
		<a
			href="/admin/shows/{id}/guests"
			class="rounded-lg border border-neutral-800 bg-neutral-900 p-4 hover:bg-neutral-800/70"
		>
			<div class="font-medium">👥 Список гостей</div>
			<div class="mt-1 text-sm text-neutral-400">
				Бронювання, скасування, CSV
			</div>
		</a>
	</section>

	<form onsubmit={save} class="mt-8 space-y-4">
		<h2 class="text-lg font-medium">Редагувати</h2>

		<div>
			<label for="t" class="block text-sm text-neutral-400">Назва</label>
			<input
				id="t"
				type="text"
				bind:value={editTitle}
				required
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			/>
		</div>
		<div>
			<label for="v" class="block text-sm text-neutral-400">Місце</label>
			<input
				id="v"
				type="text"
				bind:value={editVenue}
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			/>
		</div>
		<div>
			<label for="s" class="block text-sm text-neutral-400">Початок</label>
			<div class="mt-1">
				<DateTimePicker bind:value={editStartsAt} id="s" required disabled={!!show.archived_at} />
			</div>
		</div>
		<div>
			<label for="poster-file" class="block text-sm text-neutral-400">Постер</label>
			<div class="mt-1 flex flex-col gap-2">
				<div class="flex items-center gap-2">
					<input
						id="poster-file"
						type="file"
						accept="image/jpeg,image/png,image/webp,image/gif"
						onchange={(e: Event) => uploadPoster((e.target as HTMLInputElement).files?.[0])}
						disabled={uploading}
						class="block w-full text-sm text-neutral-300 file:mr-3 file:rounded-md file:border-0 file:bg-neutral-800 file:px-3 file:py-1.5 file:text-sm file:font-medium file:text-neutral-200 hover:file:bg-neutral-700"
					/>
					{#if uploading}
						<span class="text-xs text-neutral-500">завантажую…</span>
					{/if}
				</div>
				<details class="text-xs text-neutral-500">
					<summary class="cursor-pointer hover:text-neutral-300">або вставити URL</summary>
					<input
						type="text"
						bind:value={editPosterURL}
						placeholder="https://example.com/poster.jpg або /posters/…"
						class="mt-2 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-sm text-neutral-100 focus:border-neutral-600 focus:outline-none"
					/>
					<p class="mt-1 text-xs text-neutral-500">
						Може бути зовнішнє https://… або шлях /posters/… від upload'у вище.
					</p>
				</details>
				{#if editPosterURL}
					<img src={editPosterURL} alt="Прев'ю постера" class="max-h-48 rounded-md border border-neutral-800" onerror={(e: Event) => ((e.target as HTMLImageElement).style.display = 'none')} />
				{/if}
				{#if uploadError}
					<p class="text-xs text-red-400">{uploadError}</p>
				{/if}
			</div>
		</div>
		<div>
			<label for="desc" class="block text-sm text-neutral-400">Опис події</label>
			<textarea
				id="desc"
				bind:value={editDescription}
				rows="5"
				placeholder="Розкажи гостям, що це за подія. Можна без форматування."
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			></textarea>
		</div>
		<div>
			<label for="grp" class="block text-sm text-neutral-400">Серія сесій</label>
			<input
				id="grp"
				type="text"
				bind:value={editSessionGroup}
				maxlength="60"
				placeholder="порожньо = разова подія"
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			/>
			<p class="mt-1 text-xs text-neutral-500">
				Та сама мітка на 2+ подіях групує їх в один blok на лендингу з вибором дати.
			</p>
		</div>

		{#if error}
			<div class="rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
				{error}
			</div>
		{/if}

		<div class="flex items-center gap-3">
			<button
				type="submit"
				disabled={saving || !!show.archived_at}
				class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
			>
				{saving ? 'Зберігаю…' : 'Зберегти'}
			</button>
			{#if savedAt}
				<span class="text-xs text-neutral-500">Збережено о {savedAt.toLocaleTimeString('uk-UA')}</span>
			{/if}
		</div>
	</form>

	<!-- Pricing tiers (categories). Seats with matching `category` string
	     inherit the colour + price; admin uses bulk-edit in the layout
	     editor to assign categories to whole zones. GA shows have a
	     single uniform price — categories don't add value. -->
	{#if show.kind !== 'ga'}
	<section class="mt-10 rounded-2xl border border-neutral-800 bg-neutral-900 p-6">
		<h2 class="text-lg font-medium">Категорії місць</h2>
		<p class="mt-1 text-sm text-neutral-400">
			Зони з різними цінами (VIP, Standard, Balcony…). Колір — для мапи
			залу, ціна — для всіх місць цієї категорії одразу.
		</p>

		{#if categories.length > 0}
			<ul class="mt-4 divide-y divide-neutral-800 rounded-lg border border-neutral-800">
				{#each categories as c (c.id)}
					<li class="flex items-center gap-3 p-3">
						<span
							class="inline-block size-6 shrink-0 rounded-full border border-neutral-700"
							style="background-color: {c.color}"
							aria-hidden="true"
						></span>
						<div class="min-w-0 flex-1">
							<div class="text-sm font-medium">{c.name}</div>
							<div class="text-xs text-neutral-500">
								<span class="font-mono">{c.color}</span>
							</div>
						</div>
						<input
							type="number"
							value={c.price_kopecks / 100}
							min="0"
							step="1"
							onchange={(e: Event) =>
								updateCategoryPrice(c, Number((e.target as HTMLInputElement).value))}
							class="w-24 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1 text-right text-sm"
						/>
						<span class="text-sm text-neutral-500">₴</span>
						<button
							type="button"
							onclick={() => removeCategory(c)}
							aria-label="Видалити категорію {c.name}"
							class="ml-2 rounded px-2 py-1 text-xs text-red-400 hover:bg-red-950/40"
						>
							Видалити
						</button>
					</li>
				{/each}
			</ul>
		{:else}
			<p class="mt-4 text-sm text-neutral-500">Поки що нема категорій.</p>
		{/if}

		<form onsubmit={addCategory} class="mt-4 grid grid-cols-[1fr_auto_auto_auto] items-end gap-2">
			<div>
				<label for="catn" class="block text-xs text-neutral-400">Назва</label>
				<input
					id="catn"
					type="text"
					bind:value={catName}
					maxlength="40"
					placeholder="VIP"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-1.5 text-sm"
				/>
			</div>
			<div>
				<label for="catc" class="block text-xs text-neutral-400">Колір</label>
				<input
					id="catc"
					type="color"
					bind:value={catColor}
					class="mt-1 h-9 w-12 cursor-pointer rounded-md border border-neutral-800 bg-neutral-950"
				/>
			</div>
			<div>
				<label for="catp" class="block text-xs text-neutral-400">Ціна (₴)</label>
				<input
					id="catp"
					type="number"
					bind:value={catPrice}
					min="0"
					class="mt-1 w-24 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-right text-sm"
				/>
			</div>
			<button
				type="submit"
				disabled={catSaving}
				class="rounded-md bg-[var(--color-brand)] px-4 py-1.5 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
			>
				+ Додати
			</button>
		</form>
		{#if catError}
			<p class="mt-2 text-xs text-red-400">{catError}</p>
		{/if}
		<p class="mt-3 text-xs text-neutral-500">
			📌 Підказка: у редакторі залу можна виділити декілька місць і призначити
			їм категорію — ціни підтягнуться автоматично.
		</p>
	</section>
	{/if}

	{#if !show.archived_at}
		<div class="mt-12 border-t border-neutral-900 pt-6">
			<h2 class="text-sm font-medium text-neutral-400">Небезпечна зона</h2>
			<p class="mt-2 text-sm text-neutral-500">
				Архівована подія зникає з дашборду, але всі квитки, бронювання й історія
				лишаються у БД. Розархівувати поки що можна лише руками через sqlite.
			</p>
			<button
				onclick={archive}
				disabled={archiving}
				class="mt-3 rounded-md border border-red-900 bg-red-950/30 px-4 py-2 text-sm text-red-300 hover:bg-red-950/60 disabled:opacity-50"
			>
				{archiving ? 'Архівую…' : 'Архівувати подію'}
			</button>
		</div>
	{/if}
{/if}
