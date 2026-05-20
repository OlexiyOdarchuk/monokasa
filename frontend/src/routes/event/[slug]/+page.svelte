<script lang="ts">
	import { page } from '$app/state';
	import {
		publicApi,
		type PublicShow,
		type PublicSeat,
		type ReservationResponse,
		ApiError
	} from '$lib/api';

	const slug = $derived(page.params.slug);

	let show = $state<PublicShow | null>(null);
	let loaded = $state(false);
	let error = $state('');

	let selectedId = $state<number | null>(null);
	const selected = $derived(show?.seats.find((s) => s.id === selectedId) ?? null);

	// Reservation state. Once `success` is set the form swaps for the
	// "pay through monobank" screen with the share-friendly code.
	let buyerName = $state('');
	let buyerEmail = $state('');
	let submitting = $state(false);
	let success = $state<ReservationResponse | null>(null);

	const SEAT_R = 22;
	const PAD = 60;

	const knownColors: Record<string, string> = {
		'': '#3b82f6',
		standard: '#3b82f6',
		vip: '#f59e0b',
		balcony: '#a78bfa',
		pit: '#10b981',
		comp: '#94a3b8'
	};
	function categoryColor(c: string): string {
		const k = c.trim().toLowerCase();
		if (knownColors[k] !== undefined) return knownColors[k];
		let h = 0;
		for (let i = 0; i < k.length; i++) h = (h * 31 + k.charCodeAt(i)) >>> 0;
		return `hsl(${h % 360}, 65%, 55%)`;
	}

	const viewBox = $derived.by(() => {
		if (!show || show.seats.length === 0) return '0 0 600 400';
		let maxX = 0;
		let maxY = 0;
		for (const s of show.seats) {
			if (s.x > maxX) maxX = s.x;
			if (s.y > maxY) maxY = s.y;
		}
		return `0 0 ${maxX + PAD} ${maxY + PAD + 40}`;
	});

	async function load() {
		try {
			show = await publicApi.get<PublicShow>(`/api/public/shows/${slug}`);
		} catch (e) {
			if (e instanceof ApiError && e.status === 404) {
				error = 'Подію не знайдено або вона вже в архіві.';
			} else if (e instanceof ApiError) {
				error = e.detail || e.code;
			} else {
				error = String(e);
			}
		} finally {
			loaded = true;
		}
	}

	$effect(() => {
		void slug;
		load();
	});

	function pickSeat(s: PublicSeat) {
		if (!s.sellable || s.taken) return;
		selectedId = s.id;
	}

	async function submit(e: Event) {
		e.preventDefault();
		if (!show || !selected) return;
		submitting = true;
		error = '';
		try {
			success = await publicApi.post<ReservationResponse>('/api/public/reservations', {
				slug: show.slug,
				seat_id: selected.id,
				buyer_name: buyerName.trim(),
				buyer_email: buyerEmail.trim()
			});
		} catch (e) {
			if (e instanceof ApiError && e.code === 'seat_taken') {
				error = 'На жаль, це місце щойно зайняли. Обери інше.';
				// Refresh seat status so the canvas stops offering it.
				load();
				selectedId = null;
			} else if (e instanceof ApiError) {
				error = e.detail || e.code;
			} else {
				error = String(e);
			}
		} finally {
			submitting = false;
		}
	}

	function startsAtText(iso: string): string {
		return new Date(iso).toLocaleString('uk-UA', {
			weekday: 'long',
			day: 'numeric',
			month: 'long',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	function formatUAH(k: number): string {
		return (k / 100).toLocaleString('uk-UA', { minimumFractionDigits: 2 }) + ' ₴';
	}
</script>

<svelte:head>
	<title>{show?.title ?? 'Подія'} — monokasa</title>
	<meta name="description" content={show ? `Купити квиток на ${show.title} — ${show.venue}` : ''} />
</svelte:head>

{#if !loaded}
	<div class="grid min-h-screen place-items-center text-neutral-500">Завантажую…</div>
{:else if error && !show}
	<div class="mx-auto mt-12 max-w-md rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center">
		<h1 class="text-lg font-medium">😔</h1>
		<p class="mt-2 text-neutral-400">{error}</p>
	</div>
{:else if success && show}
	<!-- Success screen: post-reservation, pre-payment -->
	<main class="mx-auto max-w-md p-6">
		<div class="rounded-2xl border border-emerald-900 bg-emerald-950/40 p-6 text-center">
			<div class="text-3xl">🎟</div>
			<h1 class="mt-2 text-xl font-semibold">Місце за тобою на 15 хвилин</h1>
			<p class="mt-2 text-sm text-neutral-300">
				{show.title} · {startsAtText(show.starts_at)}
			</p>
			<p class="mt-1 text-sm text-neutral-400">
				ряд {success.seat.row} · місце {success.seat.col} · {formatUAH(success.seat.price_kopecks)}
			</p>

			<div class="mt-5 rounded-lg border border-neutral-800 bg-neutral-950 p-3 text-left">
				<div class="text-xs text-neutral-500">Код броні (вже у коментарі платежу):</div>
				<div class="mt-1 font-mono text-lg tracking-wider">{success.code}</div>
			</div>

			<a
				href={success.pay_url}
				target="_blank"
				rel="noopener"
				class="mt-5 block w-full rounded-lg bg-[var(--color-brand)] px-4 py-3 text-base font-medium text-black hover:bg-[var(--color-brand-hover)]"
			>
				💳 Сплатити через monobank →
			</a>

			<p class="mt-4 text-xs text-neutral-500">
				Після оплати квиток із QR прийде на <b>{success.buyer_email}</b>
				(потерпи кілька хвилин на доставку).
			</p>
		</div>
	</main>
{:else if show}
	<main class="mx-auto max-w-3xl p-4 sm:p-6">
		<header class="mb-4">
			<h1 class="text-2xl font-semibold tracking-tight sm:text-3xl">{show.title}</h1>
			<p class="mt-1 text-sm text-neutral-400">{startsAtText(show.starts_at)}</p>
			{#if show.venue}
				<p class="text-sm text-neutral-400">📍 {show.venue}</p>
			{/if}
		</header>

		<!-- canvas -->
		<div class="rounded-lg border border-neutral-800 bg-neutral-950 p-2">
			<svg
				viewBox={viewBox}
				class="block w-full max-h-[65vh] select-none"
				role="img"
				aria-label="Мапа зали"
			>
				<rect x="0" y="0" width="100%" height="32" fill="#1a1a1a" />
				<text x="50%" y="20" text-anchor="middle" fill="#9ca3af" font-size="14" font-family="system-ui">
					━━━━━ СЦЕНА ━━━━━
				</text>

				{#each show.seats as seat (seat.id)}
					{@const isSel = selectedId === seat.id}
					{@const blocked = !seat.sellable || seat.taken}
					{@const fill = blocked ? '#3a3a3a' : categoryColor(seat.category)}
					<g
						transform="translate({seat.x} {seat.y + 40})"
						role="button"
						tabindex={blocked ? -1 : 0}
						aria-label="Місце ряд {seat.row} місце {seat.col}{blocked ? ' (зайняте)' : ''}"
						class={blocked ? 'cursor-not-allowed' : 'cursor-pointer'}
						onclick={() => pickSeat(seat)}
						onkeydown={(e: KeyboardEvent) => {
							if (e.key === 'Enter' || e.key === ' ') {
								e.preventDefault();
								pickSeat(seat);
							}
						}}
					>
						<circle
							r={SEAT_R}
							fill={fill}
							fill-opacity={blocked ? 0.35 : 0.9}
							stroke={isSel ? '#ffffff' : '#0a0a0a'}
							stroke-width={isSel ? 3 : 1}
						/>
						<text
							y="5"
							text-anchor="middle"
							fill={blocked ? '#666' : '#fff'}
							font-size="11"
							font-weight="600"
							font-family="system-ui"
							pointer-events="none"
						>
							{seat.label || `${seat.row}-${seat.col}`}
						</text>
					</g>
				{/each}
			</svg>
		</div>

		<!-- legend -->
		<div class="mt-3 flex flex-wrap gap-3 text-xs text-neutral-400">
			<span class="inline-flex items-center gap-1.5">
				<span class="inline-block size-3 rounded-full bg-blue-500"></span> вільно
			</span>
			<span class="inline-flex items-center gap-1.5">
				<span class="inline-block size-3 rounded-full bg-neutral-700"></span> зайнято
			</span>
			<span class="inline-flex items-center gap-1.5">
				<span class="inline-block size-3 rounded-full border-2 border-white bg-blue-500"></span> обрано
			</span>
		</div>

		<!-- selection / form panel -->
		{#if selected}
			<form
				onsubmit={submit}
				class="mt-5 rounded-2xl border border-neutral-800 bg-neutral-900 p-5"
			>
				<div class="flex items-center justify-between gap-3">
					<div>
						<div class="text-sm text-neutral-400">Обрано місце</div>
						<div class="text-lg font-medium">
							ряд {selected.row} · місце {selected.col}
							{#if selected.category}<span class="ml-2 text-sm text-neutral-500">({selected.category})</span>{/if}
						</div>
					</div>
					<div class="text-right">
						<div class="text-xs text-neutral-500">До оплати</div>
						<div class="text-lg font-semibold">{formatUAH(selected.price_kopecks)}</div>
					</div>
				</div>

				<div class="mt-4 space-y-3">
					<div>
						<label for="n" class="block text-sm text-neutral-400">Ім'я та прізвище</label>
						<input
							id="n"
							type="text"
							bind:value={buyerName}
							required
							minlength="2"
							maxlength="60"
							placeholder="Олена Петренко"
							class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base focus:border-neutral-600 focus:outline-none"
						/>
					</div>
					<div>
						<label for="e" class="block text-sm text-neutral-400">Email для квитка</label>
						<input
							id="e"
							type="email"
							bind:value={buyerEmail}
							required
							placeholder="email@example.com"
							class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base focus:border-neutral-600 focus:outline-none"
						/>
						<p class="mt-1 text-xs text-neutral-500">
							PDF з QR прийде сюди після оплати. На вході — покажи з телефону.
						</p>
					</div>
				</div>

				{#if error}
					<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
						{error}
					</div>
				{/if}

				<div class="mt-5 flex gap-2">
					<button
						type="button"
						onclick={() => (selectedId = null)}
						class="rounded-md border border-neutral-700 px-4 py-2 text-sm text-neutral-300 hover:bg-neutral-800"
					>
						Скасувати
					</button>
					<button
						type="submit"
						disabled={submitting}
						class="flex-1 rounded-md bg-[var(--color-brand)] px-4 py-2 text-base font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
					>
						{submitting ? 'Бронюю…' : 'Забронювати і платити'}
					</button>
				</div>
			</form>
		{:else}
			<p class="mt-5 text-center text-sm text-neutral-500">
				Тицьни вільне місце на мапі вище 👆
			</p>
		{/if}
	</main>
{/if}
