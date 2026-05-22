<script lang="ts">
	import { page } from '$app/state';
	import { SvelteSet } from 'svelte/reactivity';
	import {
		publicApi,
		type PublicShow,
		type PublicSeat,
		type CreateOrderResponse,
		type ReservationStatus,
		type ReservationStatusResponse,
		ApiError
	} from '$lib/api';

	const slug = $derived(page.params.slug);

	let show = $state<PublicShow | null>(null);
	let loaded = $state(false);
	let error = $state('');

	// Multi-seat: SvelteSet so `add`/`delete` calls reactively re-trigger
	// the $derived selections. Plain `$state(new Set())` doesn't reliably
	// notify on mutation in Svelte 5.
	let selectedIds = $state(new SvelteSet<number>());
	const selectedSeats = $derived.by(() => {
		if (!show) return [] as PublicSeat[];
		const ids = selectedIds;
		// Preserve canvas order (row/col-ish via x/y already), not click order.
		return show.seats.filter((s) => ids.has(s.id));
	});
	const isGA = $derived(show?.kind === 'ga');
	// GA quantity picker. Decoupled from selectedIds so the seated path
	// stays untouched. Clamped to [1, min(SEATS_MAX, ga_free)] on input.
	let gaQuantity = $state(1);
	const gaPriceKopecks = $derived(show?.ga_price_kopecks ?? 0);
	const totalKopecks = $derived(
		isGA
			? gaPriceKopecks * gaQuantity
			: selectedSeats.reduce((acc, s) => acc + s.price_kopecks, 0)
	);

	// Scarcity nudge: pulses on the page header when very few seats
	// remain. Seated shows count free sellable seats; GA shows take the
	// ga_free counter that the server pre-computed.
	const seatsRemaining = $derived(
		isGA
			? (show?.ga_free ?? 0)
			: (show?.seats ?? []).filter((s) => s.sellable && !s.taken).length
	);

	let buyerName = $state('');
	let buyerEmail = $state('');
	// Optional promo code. Empty = no discount. Server validates +
	// applies inside the order-create tx; on error we surface the
	// reason via the existing `error` slot.
	let promoCode = $state('');
	// Optional attendee name per seat, keyed by seat id. Empty/whitespace
	// falls back to buyerName at render time. Only sent when at least one
	// entry is non-empty — otherwise the request omits the field entirely.
	let attendeeNames = $state<Record<number, string>>({});
	let showAttendees = $state(false);
	let submitting = $state(false);
	let success = $state<CreateOrderResponse | null>(null);

	// Polled by the success screen. Starts at 'held', flips to 'paid'
	// when the webhook lands the confirmation, swaps the UI for the
	// "оплачено" screen. monobank-jar doesn't redirect back to us after
	// payment, so polling is the only way to detect completion.
	let payStatus = $state<ReservationStatus>('held');

	const SEAT_R = 22;
	const PAD = 60;
	const SEATS_MAX = 20; // matches server-side soft cap on /api/public/orders

	// Admin-defined categories (with colour + price) override the
	// legacy known-colours mapping below. Fallback for unknown labels
	// is the original hashed-hue gradient — keeps old shows visually
	// stable even without categories.
	const categoryByName = $derived(
		new Map((show?.categories ?? []).map((c) => [c.name, c]))
	);
	const knownColors: Record<string, string> = {
		'': '#3b82f6',
		standard: '#3b82f6',
		vip: '#f59e0b',
		balcony: '#a78bfa',
		pit: '#10b981',
		comp: '#94a3b8'
	};
	function categoryColor(c: string): string {
		const cat = categoryByName.get(c);
		if (cat) return cat.color;
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

	// Browsers throttle background tabs aggressively — an EventSource
	// can miss frames while the page sleeps. When the tab regains
	// focus, do a one-shot refetch so the map matches reality. Skipped
	// once we're on the success/expired/paid screens (their state has
	// its own poller and a stale map there doesn't matter).
	$effect(() => {
		if (success) return;
		function refresh() {
			if (document.visibilityState === 'visible') load();
		}
		document.addEventListener('visibilitychange', refresh);
		return () => document.removeEventListener('visibilitychange', refresh);
	});

	// Live seat updates over Server-Sent Events. The server publishes a
	// "seat_status" frame whenever someone reserves/cancels/pays for a
	// seat in this show, so the picker reflects reality without polling.
	// Browser EventSource handles reconnect on its own (3s default).
	$effect(() => {
		if (!show || !loaded) return;
		const src = new EventSource(`/api/public/shows/${show.slug}/events`);
		src.onmessage = (e) => {
			try {
				const ev = JSON.parse(e.data) as {
					type: string;
					seat_id: number;
					status: 'free' | 'held' | 'sold';
				};
				if (ev.type !== 'seat_status' || !show) return;
				const idx = show.seats.findIndex((s) => s.id === ev.seat_id);
				if (idx < 0) return;
				// `taken` covers both held and sold for the picker's purposes.
				show.seats[idx] = { ...show.seats[idx], taken: ev.status !== 'free' };
				// If our basket includes this seat and it just got taken
				// by someone else, drop it so the user doesn't try to
				// submit a stale selection.
				if (ev.status !== 'free' && selectedIds.has(ev.seat_id)) {
					selectedIds.delete(ev.seat_id);
				}
			} catch (err) {
				console.warn('SSE parse failed', err);
			}
		};
		src.onerror = () => {
			// Browser auto-reconnects; nothing to do. Logging only when
			// the connection actually closes would require readyState
			// checks — for now stay quiet.
		};
		return () => src.close();
	});

	function toggleSeat(s: PublicSeat) {
		if (!s.sellable || s.taken) return;
		if (selectedIds.has(s.id)) {
			selectedIds.delete(s.id);
			return;
		}
		if (selectedIds.size >= SEATS_MAX) {
			error = `Максимум ${SEATS_MAX} місць за одну покупку.`;
			return;
		}
		error = '';
		selectedIds.add(s.id);
	}

	function clearSelection() {
		selectedIds.clear();
		error = '';
	}

	async function submit(e: Event) {
		e.preventDefault();
		if (!show) return;
		if (!isGA && selectedSeats.length === 0) return;
		if (isGA && gaQuantity < 1) return;
		submitting = true;
		error = '';
		const promo = promoCode.trim();
		try {
			if (isGA) {
				success = await publicApi.post<CreateOrderResponse>('/api/public/orders', {
					slug: show.slug,
					seat_ids: [],
					quantity: gaQuantity,
					buyer_name: buyerName.trim(),
					buyer_email: buyerEmail.trim(),
					...(promo ? { discount_code: promo } : {})
				});
			} else {
				// Build the attendee_names slice 1:1 with selected seats. Skip
				// the field entirely when every entry is empty so older code
				// paths (server logs, single-seat alias) stay clean.
				const attendees = selectedSeats.map((s) => (attendeeNames[s.id] ?? '').trim());
				const anyFilled = attendees.some((n) => n.length > 0);
				success = await publicApi.post<CreateOrderResponse>('/api/public/orders', {
					slug: show.slug,
					seat_ids: selectedSeats.map((s) => s.id),
					buyer_name: buyerName.trim(),
					buyer_email: buyerEmail.trim(),
					...(anyFilled ? { attendee_names: attendees } : {}),
					...(promo ? { discount_code: promo } : {})
				});
			}
			payStatus = 'held';
		} catch (e) {
			if (
				e instanceof ApiError &&
				(e.code === 'seat_taken' || e.code === 'seat_not_sellable' || e.code === 'not_enough_seats')
			) {
				error =
					e.code === 'not_enough_seats'
						? 'Замало вільних квитків. Зменш кількість і спробуй ще.'
						: 'Одне з обраних місць щойно зайняли. Онови мапу й обери інше.';
				load();
				if (!isGA) clearSelection();
			} else if (e instanceof ApiError) {
				error = e.detail || e.code;
			} else {
				error = String(e);
			}
		} finally {
			submitting = false;
		}
	}

	// Polling: once we have an order `code`, ping the status endpoint
	// every 5s until terminal state. Stop after 30 min as a safety net
	// against forgotten tabs — by then the HOLD has expired anyway.
	$effect(() => {
		if (!success) return;
		if (payStatus === 'paid' || payStatus === 'cancelled' || payStatus === 'expired') return;

		const code = success.code;
		const stopAt = Date.now() + 30 * 60_000;
		let cancelled = false;

		async function tick() {
			if (cancelled || Date.now() > stopAt) return;
			try {
				const r = await publicApi.get<ReservationStatusResponse>(
					`/api/public/reservations/${code}/status`
				);
				if (cancelled) return;
				payStatus = r.status;
				if (r.status !== 'held') return; // terminal — let effect re-run cleanup
			} catch (e) {
				console.warn('status poll failed', e);
			}
			setTimeout(tick, 5000);
		}
		setTimeout(tick, 5000);

		return () => {
			cancelled = true;
		};
	});

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

	// Live "через 3 дні · 14 год" counter; ticks every minute. Returns
	// '' when the event has already started so we don't show negative
	// countdown.
	let nowMs = $state(Date.now());
	$effect(() => {
		const t = setInterval(() => (nowMs = Date.now()), 60_000);
		return () => clearInterval(t);
	});
	const countdownText = $derived.by(() => {
		if (!show) return '';
		const diffMs = new Date(show.starts_at).getTime() - nowMs;
		if (diffMs <= 0) return '';
		const mins = Math.floor(diffMs / 60_000);
		if (mins < 60) return `через ${mins} хв`;
		const hours = Math.floor(mins / 60);
		if (hours < 24) return `через ${hours} год ${mins % 60} хв`;
		const days = Math.floor(hours / 24);
		const hRest = hours % 24;
		return `через ${days} ${dayWord(days)}${hRest > 0 ? ` ${hRest} год` : ''}`;
	});
	const countdownUrgent = $derived.by(() => {
		if (!show) return false;
		const diffMs = new Date(show.starts_at).getTime() - nowMs;
		return diffMs > 0 && diffMs < 24 * 60 * 60_000;
	});
	function dayWord(n: number): string {
		if (n === 1) return 'день';
		if (n >= 2 && n <= 4) return 'дні';
		return 'днів';
	}

	function formatUAH(k: number): string {
		return (k / 100).toLocaleString('uk-UA', { minimumFractionDigits: 2 }) + ' ₴';
	}

	// Waitlist: shown only when everything is sold. POST endpoint accepts
	// duplicates silently (server-side ON CONFLICT) so the form stays
	// optimistic and the user just sees "🔔 Записали тебе".
	let waitlistEmail = $state('');
	let waitlistSent = $state(false);
	let waitlistError = $state('');
	let waitlistSubmitting = $state(false);

	async function joinWaitlist(e: Event) {
		e.preventDefault();
		if (!show) return;
		waitlistSubmitting = true;
		waitlistError = '';
		try {
			await publicApi.post('/api/public/waitlist', {
				slug: show.slug,
				email: waitlistEmail.trim()
			});
			waitlistSent = true;
		} catch (e) {
			if (e instanceof ApiError) waitlistError = e.detail || e.code;
			else waitlistError = String(e);
		} finally {
			waitlistSubmitting = false;
		}
	}

	function seatLabel(s: PublicSeat): string {
		if (s.category === 'GA') return `вхід · квиток №${s.col}`;
		return `ряд ${s.row} · місце ${s.col}`;
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
{:else if success && show && payStatus === 'paid'}
	<!-- Confirmed: webhook landed, tickets sent to email/telegram -->
	<main class="mx-auto max-w-md p-6">
		<div class="rounded-2xl border border-emerald-700 bg-emerald-950/60 p-6 text-center">
			<div class="text-5xl">🎉</div>
			<h1 class="mt-3 text-2xl font-semibold">Оплачено!</h1>
			<p class="mt-2 text-sm text-emerald-200">
				{show.title} · {startsAtText(show.starts_at)}
			</p>
			<ul class="mt-3 space-y-1 text-sm text-emerald-300/80">
				{#each success.items as it (it.seat.id)}
					<li>· {seatLabel(it.seat)}</li>
				{/each}
			</ul>
			<div class="mt-5 rounded-lg border border-emerald-900 bg-emerald-950 p-4 text-left">
				<p class="text-sm text-emerald-200">
					📧 {success.items.length > 1 ? `${success.items.length} квитки` : 'Квиток'} із QR-кодом
					надіслано на <b>{success.buyer_email}</b>.
				</p>
				<p class="mt-2 text-xs text-emerald-300/70">
					На вході відкривай PDF з телефону — охорона сканує QR.
				</p>
			</div>
			<div class="mt-5 flex flex-col items-center gap-2">
				<a
					href="/my"
					class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)]"
				>
					🎟 Подивитися всі мої квитки
				</a>
				<a href="/" class="text-sm text-emerald-300 hover:underline">
					← До списку подій
				</a>
			</div>
		</div>
	</main>
{:else if success && show && (payStatus === 'expired' || payStatus === 'cancelled')}
	<!-- HOLD timed out before payment landed, or admin cancelled -->
	<main class="mx-auto max-w-md p-6">
		<div class="rounded-2xl border border-neutral-800 bg-neutral-900 p-6 text-center">
			<div class="text-3xl">⏰</div>
			<h1 class="mt-2 text-xl font-semibold">
				{payStatus === 'expired' ? 'Час бронювання вийшов' : 'Бронювання скасовано'}
			</h1>
			<p class="mt-2 text-sm text-neutral-400">
				{#if payStatus === 'expired'}
					Бронь жила 15 хв і пропала, бо оплата так і не дійшла. Можеш забронити те саме місце знов.
				{:else}
					Цю бронь було скасовано. Якщо ти оплатив після цього — напиши організатору.
				{/if}
			</p>
			<button
				onclick={() => {
					success = null;
					payStatus = 'held';
					clearSelection();
					load();
				}}
				class="mt-5 rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)]"
			>
				Спробувати ще раз
			</button>
		</div>
	</main>
{:else if success && show}
	<!-- Held: post-order, awaiting payment. Polling runs in background. -->
	<main class="mx-auto max-w-md p-6">
		<div class="rounded-2xl border border-emerald-900 bg-emerald-950/40 p-6 text-center">
			<div class="text-3xl">🎟</div>
			<h1 class="mt-2 text-xl font-semibold">
				{success.items.length > 1
					? `${success.items.length} місця за тобою на 15 хвилин`
					: 'Місце за тобою на 15 хвилин'}
			</h1>
			<p class="mt-2 text-sm text-neutral-300">
				{show.title} · {startsAtText(show.starts_at)}
			</p>

			<ul class="mt-3 space-y-1 text-left text-sm text-neutral-400">
				{#each success.items as it (it.seat.id)}
					<li class="flex justify-between rounded border border-neutral-800 bg-neutral-950 px-3 py-1.5">
						<span>{seatLabel(it.seat)}{it.seat.category ? ` · ${it.seat.category}` : ''}</span>
						<span class="text-neutral-500">{formatUAH(it.seat.price_kopecks)}</span>
					</li>
				{/each}
			</ul>

			{#if success.discount_kopecks && success.discount_kopecks > 0}
				<div class="mt-4 flex items-center justify-between rounded-lg border border-emerald-900 bg-emerald-950/40 px-3 py-2 text-left">
					<span class="text-sm text-emerald-300">🏷 {success.discount_code}</span>
					<span class="text-sm font-semibold text-emerald-300">−{formatUAH(success.discount_kopecks)}</span>
				</div>
			{/if}
			<div class="mt-2 flex items-center justify-between rounded-lg border border-neutral-800 bg-neutral-950 px-3 py-2 text-left">
				<span class="text-sm text-neutral-400">До оплати</span>
				<span class="text-base font-semibold">{formatUAH(success.total_kopecks)}</span>
			</div>

			<div class="mt-3 rounded-lg border border-neutral-800 bg-neutral-950 p-3 text-left">
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

			{#if success.tg_deep_link}
				<a
					href={success.tg_deep_link}
					target="_blank"
					rel="noopener"
					class="mt-3 block w-full rounded-lg border border-sky-700 bg-sky-950/40 px-4 py-3 text-sm text-sky-200 hover:bg-sky-950/70"
				>
					💬 Підключити Telegram (квитки ще й сюди)
				</a>
				<p class="mt-2 text-xs text-neutral-500">
					Опційно. Якщо підключиш — після оплати PDF прийде і на email, і в Telegram.
				</p>
			{/if}

			<div class="mt-4 flex items-center justify-center gap-2 text-xs text-neutral-500">
				<span class="inline-block size-2 animate-pulse rounded-full bg-amber-400"></span>
				Чекаю підтвердження оплати… (оновлюю кожні 5с)
			</div>
		</div>
	</main>
{:else if show}
	<main class="mx-auto max-w-3xl p-4 sm:p-6 pb-32">
		{#if show.poster_url}
			<div class="mb-4 overflow-hidden rounded-xl border border-neutral-800 bg-neutral-950">
				<img
					src={show.poster_url}
					alt={show.title}
					class="aspect-[16/9] w-full object-cover"
					onerror={(e: Event) => ((e.target as HTMLImageElement).style.display = 'none')}
				/>
			</div>
		{/if}
		<header class="mb-4">
			<h1 class="text-2xl font-semibold tracking-tight sm:text-3xl">{show.title}</h1>
			<p class="mt-1 text-sm text-neutral-400">{startsAtText(show.starts_at)}</p>
			{#if countdownText}
				<p class="mt-1 text-sm {countdownUrgent ? 'font-semibold text-amber-400' : 'text-neutral-500'}">
					⏳ {countdownText}
				</p>
			{/if}
			{#if show.venue}
				<p class="text-sm text-neutral-400">📍 {show.venue}</p>
			{/if}
			{#if show.description}
				<p class="mt-3 whitespace-pre-line text-sm text-neutral-300">{show.description}</p>
			{/if}
			{#if seatsRemaining > 0 && seatsRemaining <= 3}
				<div class="mt-3 inline-flex animate-pulse items-center gap-2 rounded-md border border-red-700 bg-red-950/50 px-3 py-1.5 text-sm font-medium text-red-300">
					🔥 Залишилось {seatsRemaining}
					{#if isGA}
						{seatsRemaining === 1 ? 'квиток' : seatsRemaining < 5 ? 'квитки' : 'квитків'}!
					{:else}
						{seatsRemaining === 1 ? 'місце' : seatsRemaining < 5 ? 'місця' : 'місць'}!
					{/if}
				</div>
			{/if}
			{#if show.siblings && show.siblings.length > 0}
				<div class="mt-4 rounded-md border border-neutral-800 bg-neutral-900/50 p-3">
					<div class="text-xs uppercase tracking-wider text-neutral-500">Інші дати цієї події</div>
					<ul class="mt-2 flex flex-wrap gap-2">
						{#each show.siblings as sib (sib.slug)}
							<li>
								<a
									href="/event/{sib.slug}"
									class="inline-flex items-center gap-2 rounded-md border border-neutral-700 bg-neutral-950 px-3 py-1.5 text-sm hover:bg-neutral-800"
								>
									<span>{startsAtText(sib.starts_at)}</span>
									{#if sib.seats_free === 0}
										<span class="rounded bg-neutral-800 px-1.5 text-xs text-neutral-500">зайнято</span>
									{:else if sib.seats_free <= 5}
										<span class="text-xs text-amber-400">{sib.seats_free} вільно</span>
									{/if}
								</a>
							</li>
						{/each}
					</ul>
				</div>
			{/if}
		</header>

		{#if seatsRemaining === 0}
			<!-- Sold out. Offer waitlist signup; server dedupes on (show, email). -->
			<section class="rounded-2xl border border-neutral-800 bg-neutral-900 p-5 text-center">
				<div class="text-3xl">😔</div>
				<h2 class="mt-2 text-lg font-semibold">Усе зайнято</h2>
				<p class="mt-1 text-sm text-neutral-400">
					Тільки хтось зніме бронь — пришлемо листа. Перший, хто встигне забронювати, виграє.
				</p>

				{#if waitlistSent}
					<div class="mt-4 inline-flex items-center gap-2 rounded-md border border-emerald-700 bg-emerald-950/40 px-4 py-2 text-sm text-emerald-200">
						🔔 Записали тебе. Чекай листа.
					</div>
				{:else}
					<form onsubmit={joinWaitlist} class="mt-4 flex flex-col items-stretch gap-2 sm:flex-row sm:justify-center">
						<input
							type="email"
							required
							bind:value={waitlistEmail}
							placeholder="email@example.com"
							class="w-full max-w-xs rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 focus:border-neutral-600 focus:outline-none"
						/>
						<button
							type="submit"
							disabled={waitlistSubmitting}
							class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
						>
							{waitlistSubmitting ? '…' : '🔔 Сповістіть мене'}
						</button>
					</form>
					{#if waitlistError}
						<p class="mt-2 text-sm text-red-300">{waitlistError}</p>
					{/if}
				{/if}
			</section>
		{:else if isGA}
			<!-- GA: quantity picker + buyer form in one panel. No seat map. -->
			<form
				onsubmit={submit}
				class="rounded-2xl border border-neutral-800 bg-neutral-900 p-5"
			>
				<div class="text-sm text-neutral-400">
					Вільно ще {seatsRemaining}
					{seatsRemaining === 1 ? 'квиток' : seatsRemaining < 5 ? 'квитки' : 'квитків'}
					з {show.ga_capacity ?? 0}
				</div>

				<div class="mt-4">
					<span class="block text-sm text-neutral-400">Скільки квитків</span>
					<div class="mt-2 flex items-center gap-3">
						<button
							type="button"
							onclick={() => (gaQuantity = Math.max(1, gaQuantity - 1))}
							disabled={gaQuantity <= 1}
							class="size-10 rounded-md border border-neutral-700 text-lg font-semibold hover:bg-neutral-800 disabled:opacity-40"
							aria-label="Менше квитків"
						>
							−
						</button>
						<input
							type="number"
							min="1"
							max={Math.min(SEATS_MAX, seatsRemaining)}
							bind:value={gaQuantity}
							class="w-20 rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-center text-lg font-semibold focus:border-neutral-600 focus:outline-none"
							aria-label="Кількість квитків"
						/>
						<button
							type="button"
							onclick={() =>
								(gaQuantity = Math.min(SEATS_MAX, seatsRemaining, gaQuantity + 1))}
							disabled={gaQuantity >= Math.min(SEATS_MAX, seatsRemaining)}
							class="size-10 rounded-md border border-neutral-700 text-lg font-semibold hover:bg-neutral-800 disabled:opacity-40"
							aria-label="Більше квитків"
						>
							+
						</button>
						<span class="ml-2 text-sm text-neutral-500">
							× {formatUAH(gaPriceKopecks)}
						</span>
					</div>
					<p class="mt-1 text-xs text-neutral-500">
						Місця не фіксовані — кожен квиток дає вхід.
					</p>
				</div>

				<div class="mt-4 flex items-center justify-between rounded-lg border border-neutral-800 bg-neutral-950 px-3 py-2">
					<span class="text-sm text-neutral-400">До оплати</span>
					<span class="text-lg font-semibold">{formatUAH(totalKopecks)}</span>
				</div>

				<div class="mt-4 space-y-3">
					<div>
						<label for="n-ga" class="block text-sm text-neutral-400">Ім'я та прізвище</label>
						<input
							id="n-ga"
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
						<label for="e-ga" class="block text-sm text-neutral-400">Email для квитків</label>
						<input
							id="e-ga"
							type="email"
							bind:value={buyerEmail}
							required
							placeholder="email@example.com"
							class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base focus:border-neutral-600 focus:outline-none"
						/>
						<p class="mt-1 text-xs text-neutral-500">
							{gaQuantity > 1 ? `${gaQuantity} PDF з QR прийдуть сюди` : 'PDF з QR прийде сюди'} після оплати.
						</p>
					</div>
					<details class="mt-1 text-sm">
						<summary class="cursor-pointer text-neutral-400 hover:text-neutral-200">🏷 У мене є промокод</summary>
						<input
							type="text"
							bind:value={promoCode}
							maxlength="40"
							placeholder="EARLYBIRD"
							class="mt-2 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base uppercase focus:border-neutral-600 focus:outline-none"
						/>
					</details>
				</div>

				{#if error}
					<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
						{error}
					</div>
				{/if}

				<button
					type="submit"
					disabled={submitting || seatsRemaining === 0}
					class="mt-5 w-full rounded-md bg-[var(--color-brand)] px-4 py-3 text-base font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
				>
					{submitting
						? 'Бронюю…'
						: seatsRemaining === 0
							? 'Квитків більше нема'
							: `Забронювати ${gaQuantity > 1 ? `${gaQuantity} і платити` : 'і платити'}`}
				</button>
			</form>
		{:else}
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
					{@const isSel = selectedIds.has(seat.id)}
					{@const blocked = !seat.sellable || seat.taken}
					{@const fill = blocked ? '#3a3a3a' : categoryColor(seat.category)}
					<g
						transform="translate({seat.x} {seat.y + 40})"
						role="button"
						tabindex={blocked ? -1 : 0}
						aria-label="Місце ряд {seat.row} місце {seat.col}{blocked ? ' (зайняте)' : isSel ? ' (обрано)' : ''}"
						aria-pressed={isSel}
						class={blocked ? 'cursor-not-allowed' : 'cursor-pointer'}
						onclick={() => toggleSeat(seat)}
						onkeydown={(e: KeyboardEvent) => {
							if (e.key === 'Enter' || e.key === ' ') {
								e.preventDefault();
								toggleSeat(seat);
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
		<div class="mt-3 space-y-2 text-xs text-neutral-400">
			{#if show.categories && show.categories.length > 0}
				<div class="flex flex-wrap gap-x-3 gap-y-1">
					{#each show.categories as cat (cat.name)}
						<span class="inline-flex items-center gap-1.5">
							<span
								class="inline-block size-3 rounded-full"
								style="background-color: {cat.color}"
							></span>
							{cat.name} · {formatUAH(cat.price_kopecks)}
						</span>
					{/each}
				</div>
			{/if}
			<div class="flex flex-wrap gap-3">
				<span class="inline-flex items-center gap-1.5">
					<span class="inline-block size-3 rounded-full bg-neutral-700"></span> зайнято
				</span>
				<span class="inline-flex items-center gap-1.5">
					<span class="inline-block size-3 rounded-full border-2 border-white bg-blue-500"></span> обрано
				</span>
			</div>
		</div>

		<!-- selection / form panel -->
		{#if selectedSeats.length > 0}
			<form
				onsubmit={submit}
				class="mt-5 rounded-2xl border border-neutral-800 bg-neutral-900 p-5"
			>
				<div class="flex items-start justify-between gap-3">
					<div class="min-w-0 flex-1">
						<div class="text-sm text-neutral-400">
							Обрано {selectedSeats.length}
							{selectedSeats.length === 1 ? 'місце' : selectedSeats.length < 5 ? 'місця' : 'місць'}
						</div>
						<ul class="mt-2 space-y-2 text-sm">
							{#each selectedSeats as s (s.id)}
								<li class="space-y-1">
									<div class="flex items-center justify-between gap-2">
										<span class="truncate">
											{seatLabel(s)}{s.category ? ` · ${s.category}` : ''}
										</span>
										<span class="flex items-center gap-2 text-neutral-500">
											<span>{formatUAH(s.price_kopecks)}</span>
											<button
												type="button"
												aria-label="Прибрати місце ряд {s.row} місце {s.col}"
												onclick={() => selectedIds.delete(s.id)}
												class="rounded px-1.5 text-neutral-500 hover:bg-neutral-800 hover:text-neutral-200"
											>
												✕
											</button>
										</span>
									</div>
									{#if showAttendees && selectedSeats.length > 1}
										<input
											type="text"
											maxlength="60"
											value={attendeeNames[s.id] ?? ''}
											oninput={(e: Event) => {
												attendeeNames[s.id] = (e.target as HTMLInputElement).value;
											}}
											placeholder={buyerName.trim() || "ім'я на квитку"}
											aria-label="Ім'я на квитку ряд {s.row} місце {s.col}"
											class="w-full rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm focus:border-neutral-600 focus:outline-none"
										/>
									{/if}
								</li>
							{/each}
						</ul>
						{#if selectedSeats.length > 1}
							<button
								type="button"
								onclick={() => (showAttendees = !showAttendees)}
								class="mt-2 text-xs text-neutral-400 hover:text-neutral-200"
							>
								{showAttendees
									? '— Один на всіх квитках'
									: '✏️ Підписати квитки на різні імена'}
							</button>
						{/if}
					</div>
					<div class="shrink-0 text-right">
						<div class="text-xs text-neutral-500">До оплати</div>
						<div class="text-lg font-semibold">{formatUAH(totalKopecks)}</div>
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
					<details class="text-sm">
						<summary class="cursor-pointer text-neutral-400 hover:text-neutral-200">🏷 У мене є промокод</summary>
						<input
							type="text"
							bind:value={promoCode}
							maxlength="40"
							placeholder="EARLYBIRD"
							class="mt-2 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base uppercase focus:border-neutral-600 focus:outline-none"
						/>
					</details>
				</div>

				{#if error}
					<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
						{error}
					</div>
				{/if}

				<div class="mt-5 flex gap-2">
					<button
						type="button"
						onclick={clearSelection}
						class="rounded-md border border-neutral-700 px-4 py-2 text-sm text-neutral-300 hover:bg-neutral-800"
					>
						Очистити
					</button>
					<button
						type="submit"
						disabled={submitting}
						class="flex-1 rounded-md bg-[var(--color-brand)] px-4 py-2 text-base font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
					>
						{submitting
							? 'Бронюю…'
							: `Забронювати ${selectedSeats.length === 1 ? 'і платити' : `${selectedSeats.length} і платити`}`}
					</button>
				</div>
			</form>
		{:else}
			<p class="mt-5 text-center text-sm text-neutral-500">
				Тицьни вільні місця на мапі вище 👆 (можна декілька)
			</p>
			{#if error}
				<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
					{error}
				</div>
			{/if}
		{/if}
		{/if}

		<footer class="mt-12 border-t border-neutral-900 pt-4 pb-2 text-center text-xs text-neutral-600">
			<a href="/about" class="hover:text-neutral-400">Про організатора</a>
		</footer>
	</main>
{/if}
