<script lang="ts">
	import { page } from '$app/state';
	import { goto } from '$app/navigation';
	import { api, type Show, type UpdateShowInput, ApiError } from '$lib/api';
	import DateTimePicker from '$lib/DateTimePicker.svelte';

	const id = $derived(Number(page.params.id));

	let show = $state<Show | null>(null);
	let loaded = $state(false);
	let error = $state('');

	// Edit form state (mirrors show fields; populated when show loads).
	let editTitle = $state('');
	let editVenue = $state('');
	let editStartsAt = $state(''); // RFC3339 UTC, two-way bound to DateTimePicker
	let editDescription = $state('');
	let editPosterURL = $state('');
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

	async function load() {
		try {
			show = await api.get<Show>(`/api/admin/shows/${id}`);
			editTitle = show.title;
			editVenue = show.venue;
			editStartsAt = show.starts_at;
			editDescription = show.description;
			editPosterURL = show.poster_url;
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			loaded = true;
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
		<a
			href="/admin/shows/{id}/layout"
			class="rounded-lg border border-neutral-800 bg-neutral-900 p-4 hover:bg-neutral-800/70"
		>
			<div class="font-medium">🎭 Редактор залу</div>
			<div class="mt-1 text-sm text-neutral-400">
				Drag&amp;drop місць, категорії, ціни — <span class="text-neutral-500">PR #4c</span>
			</div>
		</a>
		<a
			href="/admin/shows/{id}/guests"
			class="rounded-lg border border-neutral-800 bg-neutral-900 p-4 hover:bg-neutral-800/70"
		>
			<div class="font-medium">👥 Список гостей</div>
			<div class="mt-1 text-sm text-neutral-400">
				Бронювання, скасування, CSV — <span class="text-neutral-500">PR #4d</span>
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
