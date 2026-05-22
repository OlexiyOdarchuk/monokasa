<script lang="ts">
	import { page } from '$app/state';
	import { api, type Guest, type Reservation, type SeatBrief, ApiError } from '$lib/api';

	const showId = $derived(Number(page.params.id));

	let guests = $state<Guest[]>([]);
	let loaded = $state(false);
	let error = $state('');

	let search = $state('');
	let statusFilter = $state<'all' | Reservation['status']>('all');

	async function load() {
		try {
			guests = await api.get<Guest[]>(`/api/admin/shows/${showId}/guests`);
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			loaded = true;
		}
	}

	$effect(() => {
		void showId;
		load();
	});

	const filtered = $derived.by(() => {
		const q = search.trim().toLowerCase();
		return guests.filter((g) => {
			if (statusFilter !== 'all' && g.reservation.status !== statusFilter) return false;
			if (q && !matches(g, q)) return false;
			return true;
		});
	});

	function matches(g: Guest, q: string): boolean {
		return (
			g.reservation.buyer_name.toLowerCase().includes(q) ||
			g.reservation.code.toLowerCase().includes(q) ||
			seatLabel(g.seat).toLowerCase().includes(q)
		);
	}

	function seatLabel(s: SeatBrief): string {
		return s.label || `ряд ${s.row} · ${s.col}`;
	}

	async function cancel(g: Guest) {
		const label = g.reservation.status === 'paid' ? 'оплачену' : '';
		if (
			!confirm(
				`Скасувати ${label} бронь ${g.reservation.buyer_name} · ${seatLabel(g.seat)}?\n\n` +
					(g.reservation.status === 'paid'
						? 'Це лише відмітка у БД. Гроші повертаєш через monobank вручну.'
						: 'Бронь зникне, місце звільниться.')
			)
		)
			return;
		try {
			const updated = await api.post<Guest>(`/api/admin/reservations/${g.reservation.id}/cancel`);
			// Replace in-place so the row updates without a full reload.
			guests = guests.map((x) => (x.reservation.id === updated.reservation.id ? updated : x));
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		}
	}

	// Refund mark — purely bookkeeping. Doesn't free seat, doesn't change
	// status. After click, the row carries refunded_at so the badge
	// replaces the button.
	async function markRefunded(g: Guest) {
		if (
			!confirm(
				`Позначити як повернуто гроші для ${g.reservation.buyer_name} · ${seatLabel(g.seat)}?\n\n` +
					'Це лише відмітка — реальний refund треба зробити руками у monobank.'
			)
		)
			return;
		try {
			await api.post(`/api/admin/reservations/${g.reservation.id}/refund`);
			// Optimistic: stamp the row locally. The endpoint returns the
			// order, not the guest row, so a full update would need a
			// refetch; one bit of state is enough for the badge.
			guests = guests.map((x) =>
				x.reservation.id === g.reservation.id
					? { ...x, reservation: { ...x.reservation, refunded_at: new Date().toISOString() } }
					: x
			);
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		}
	}

	function formatDateTime(iso: string | null | undefined): string {
		if (!iso) return '—';
		return new Date(iso).toLocaleString('uk-UA', {
			day: '2-digit',
			month: '2-digit',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit'
		});
	}

	function formatUAH(k: number): string {
		return (k / 100).toLocaleString('uk-UA', { minimumFractionDigits: 2 }) + ' ₴';
	}

	// Status badge styling. Tailwind palette tuned so all four read at a
	// glance: green=good, amber=pending, neutral=stale, red=cancelled.
	const statusStyles: Record<Reservation['status'], { label: string; cls: string }> = {
		paid: { label: 'оплачено', cls: 'bg-emerald-950/50 text-emerald-300 border-emerald-900' },
		held: { label: 'чекає оплати', cls: 'bg-amber-950/50 text-amber-300 border-amber-900' },
		expired: { label: 'термін минув', cls: 'bg-neutral-800 text-neutral-400 border-neutral-700' },
		cancelled: { label: 'скасовано', cls: 'bg-red-950/50 text-red-300 border-red-900' }
	};

	// Counts per status so the filter chips show "Усі (15) · Оплачено (10) …"
	const counts = $derived.by(() => {
		const c = { all: guests.length, paid: 0, held: 0, expired: 0, cancelled: 0 };
		for (const g of guests) c[g.reservation.status]++;
		return c;
	});
</script>

<svelte:head>
	<title>monokasa · гості</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin/shows/{showId}" class="text-sm text-neutral-400 hover:text-neutral-200"
		>← До події</a
	>
</div>

<div class="mt-2 flex flex-wrap items-baseline justify-between gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">Гості</h1>
	<a
		href="/api/admin/shows/{showId}/guests.csv"
		class="rounded-md border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
	>
		⬇ CSV
	</a>
</div>

{#if error}
	<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
		{error}
	</div>
{/if}

<div class="mt-4 flex flex-wrap items-center gap-2">
	<!-- search -->
	<input
		type="text"
		placeholder="Пошук по імені, коду або місцю…"
		bind:value={search}
		class="min-w-64 flex-1 rounded-md border border-neutral-800 bg-neutral-900 px-3 py-1.5 text-sm focus:border-neutral-600 focus:outline-none"
	/>
	<!-- status chips -->
	<div class="flex gap-1">
		{#each ['all', 'paid', 'held', 'expired', 'cancelled'] as const as s (s)}
			<button
				onclick={() => (statusFilter = s)}
				class="rounded-md border px-3 py-1.5 text-xs {statusFilter === s
					? 'border-[var(--color-brand)] bg-[var(--color-brand)]/15 text-neutral-100'
					: 'border-neutral-800 bg-neutral-900 text-neutral-400 hover:bg-neutral-800'}"
			>
				{s === 'all' ? 'Усі' : statusStyles[s].label} ({counts[s]})
			</button>
		{/each}
	</div>
</div>

{#if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else if filtered.length === 0}
	<div class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center text-neutral-400">
		{#if guests.length === 0}
			Ще немає бронювань.
		{:else}
			Нічого не знайдено за фільтром.
		{/if}
	</div>
{:else}
	<div class="mt-4 overflow-x-auto rounded-lg border border-neutral-800">
		<table class="w-full text-sm">
			<thead class="bg-neutral-900 text-left text-xs uppercase tracking-wider text-neutral-500">
				<tr>
					<th class="px-3 py-2">Код</th>
					<th class="px-3 py-2">Гість</th>
					<th class="px-3 py-2">Місце</th>
					<th class="px-3 py-2 text-right">Ціна</th>
					<th class="px-3 py-2">Статус</th>
					<th class="px-3 py-2">Створено</th>
					<th class="px-3 py-2">Оплачено</th>
					<th class="px-3 py-2"></th>
				</tr>
			</thead>
			<tbody class="divide-y divide-neutral-900">
				{#each filtered as g (g.reservation.id)}
					{@const style = statusStyles[g.reservation.status]}
					<tr class="hover:bg-neutral-900/50">
						<td class="px-3 py-2 font-mono text-xs text-neutral-400">{g.reservation.code}</td>
						<td class="px-3 py-2">{g.reservation.buyer_name}</td>
						<td class="px-3 py-2">{seatLabel(g.seat)}</td>
						<td class="px-3 py-2 text-right">{formatUAH(g.seat.price_kopecks)}</td>
						<td class="px-3 py-2">
							<span class="inline-block rounded border px-2 py-0.5 text-xs {style.cls}">
								{style.label}
							</span>
							{#if g.reservation.refunded_at}
								<span class="ml-1 inline-block rounded border border-violet-900 bg-violet-950/50 px-2 py-0.5 text-xs text-violet-300">
									повернуто
								</span>
							{/if}
						</td>
						<td class="px-3 py-2 text-xs text-neutral-500">{formatDateTime(g.reservation.created_at)}</td>
						<td class="px-3 py-2 text-xs text-neutral-500">{formatDateTime(g.reservation.confirmed_at)}</td>
						<td class="px-3 py-2 text-right">
							<div class="flex justify-end gap-3">
								{#if g.reservation.confirmed_at && !g.reservation.refunded_at}
									<button
										onclick={() => markRefunded(g)}
										class="text-xs text-violet-400 hover:underline"
									>
										Повернути
									</button>
								{/if}
								{#if g.reservation.status !== 'cancelled'}
									<button
										onclick={() => cancel(g)}
										class="text-xs text-red-400 hover:underline"
									>
										Скасувати
									</button>
								{/if}
							</div>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
