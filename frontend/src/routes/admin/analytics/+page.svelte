<script lang="ts">
	import { api, type Analytics, ApiError } from '$lib/api';

	let data = $state<Analytics | null>(null);
	let loaded = $state(false);
	let error = $state('');
	let days = $state(30);

	async function load() {
		loaded = false;
		error = '';
		try {
			data = await api.get<Analytics>(`/api/admin/analytics?days=${days}`);
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			loaded = true;
		}
	}

	$effect(() => {
		void days;
		load();
	});

	function formatUAH(kopecks: number): string {
		return (kopecks / 100).toLocaleString('uk-UA', {
			style: 'currency',
			currency: 'UAH',
			maximumFractionDigits: 0
		});
	}

	// Bar chart sizing. SVG viewBox stays the same; we just space bars by
	// share of width so 7 days and 90 days both fit. Y axis = revenue
	// (more dramatic than ticket count, and that's what the organizer
	// actually cares about).
	const chartW = 800;
	const chartH = 220;
	const padL = 40;
	const padR = 8;
	const padT = 12;
	const padB = 22;

	const maxRev = $derived(
		Math.max(1, ...((data?.daily_sales ?? []).map((d) => d.revenue_kopecks)))
	);

	function barX(idx: number, total: number): number {
		const innerW = chartW - padL - padR;
		const slot = innerW / Math.max(1, total);
		return padL + idx * slot + slot * 0.15;
	}
	function barW(total: number): number {
		const innerW = chartW - padL - padR;
		const slot = innerW / Math.max(1, total);
		return slot * 0.7;
	}
	function barH(revenue: number): number {
		const innerH = chartH - padT - padB;
		return (revenue / maxRev) * innerH;
	}
	function barY(revenue: number): number {
		return chartH - padB - barH(revenue);
	}

	function shortDate(iso: string): string {
		// "YYYY-MM-DD" → "DD.MM"
		const [, mo, da] = iso.split('-');
		return `${da}.${mo}`;
	}
</script>

<svelte:head>
	<title>monokasa · аналітика</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← Назад</a>
</div>
<div class="mt-2 flex items-center justify-between gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">Аналітика</h1>
	<select
		bind:value={days}
		onchange={() => load()}
		class="rounded-md border border-neutral-700 bg-neutral-900 px-3 py-2 text-sm"
	>
		<option value={7}>Останні 7 днів</option>
		<option value={30}>Останні 30 днів</option>
		<option value={90}>Останні 90 днів</option>
		<option value={365}>Останній рік</option>
	</select>
</div>

{#if error}
	<div class="mt-6 rounded-md border border-red-900 bg-red-950/50 p-4 text-sm text-red-300">
		{error}
	</div>
{:else if !loaded || !data}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else}
	<!-- KPI cards -->
	<section class="mt-6 grid grid-cols-2 gap-3 sm:grid-cols-4">
		<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			<div class="text-xs text-neutral-500">Виторг</div>
			<div class="mt-1 text-2xl font-semibold text-emerald-400">
				{formatUAH(data.total_revenue_kopecks)}
			</div>
		</div>
		<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			<div class="text-xs text-neutral-500">Квитків продано</div>
			<div class="mt-1 text-2xl font-semibold">{data.total_tickets}</div>
		</div>
		<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			<div class="text-xs text-neutral-500">Замовлень</div>
			<div class="mt-1 text-2xl font-semibold">
				{data.orders_paid}
				<span class="text-base text-neutral-500">/ {data.orders_created}</span>
			</div>
		</div>
		<div class="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			<div class="text-xs text-neutral-500">Конверсія</div>
			<div class="mt-1 text-2xl font-semibold">
				{data.conversion_percent.toFixed(0)}%
			</div>
		</div>
	</section>

	<!-- Daily revenue chart -->
	<section class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900 p-4">
		<h2 class="text-sm font-medium text-neutral-300">Виторг по днях</h2>
		{#if data.total_revenue_kopecks === 0}
			<p class="mt-3 text-sm text-neutral-500">
				За обраний період ще не було оплачених замовлень.
			</p>
		{:else}
			<svg
				viewBox="0 0 {chartW} {chartH}"
				class="mt-3 block w-full"
				role="img"
				aria-label="Виторг по днях"
			>
				<!-- Y axis baseline -->
				<line
					x1={padL}
					y1={chartH - padB}
					x2={chartW - padR}
					y2={chartH - padB}
					stroke="#404040"
					stroke-width="1"
				/>
				<!-- Y axis labels: 0 and max -->
				<text x="4" y={chartH - padB + 4} fill="#a3a3a3" font-size="10" font-family="system-ui">
					0
				</text>
				<text x="4" y={padT + 6} fill="#a3a3a3" font-size="10" font-family="system-ui">
					{formatUAH(maxRev)}
				</text>

				{#each data.daily_sales as d, i (d.date)}
					<rect
						x={barX(i, data.daily_sales.length)}
						y={barY(d.revenue_kopecks)}
						width={barW(data.daily_sales.length)}
						height={barH(d.revenue_kopecks)}
						fill="#10b981"
						opacity={d.revenue_kopecks > 0 ? 0.85 : 0.15}
					>
						<title
							>{d.date} · {d.tickets} квит. · {formatUAH(d.revenue_kopecks)}</title
						>
					</rect>
				{/each}

				<!-- X labels: every Nth date so they don't pile up -->
				{#each data.daily_sales as d, i (`l-${d.date}`)}
					{#if i % Math.max(1, Math.floor(data.daily_sales.length / 8)) === 0 || i === data.daily_sales.length - 1}
						<text
							x={barX(i, data.daily_sales.length) + barW(data.daily_sales.length) / 2}
							y={chartH - 6}
							fill="#737373"
							font-size="10"
							font-family="system-ui"
							text-anchor="middle"
						>
							{shortDate(d.date)}
						</text>
					{/if}
				{/each}
			</svg>
		{/if}
	</section>

	<!-- Per-show table -->
	<section class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900">
		<h2 class="border-b border-neutral-800 p-4 text-sm font-medium text-neutral-300">
			По подіях
		</h2>
		{#if data.per_show.length === 0}
			<p class="p-4 text-sm text-neutral-500">Активних подій нема.</p>
		{:else}
			<div class="overflow-x-auto">
				<table class="w-full text-sm">
					<thead class="text-xs uppercase text-neutral-500">
						<tr>
							<th class="px-4 py-2 text-left">Подія</th>
							<th class="px-4 py-2 text-right">Продано</th>
							<th class="px-4 py-2 text-right">Тримається</th>
							<th class="px-4 py-2 text-right">Вільно</th>
							<th class="px-4 py-2 text-right">Виторг</th>
						</tr>
					</thead>
					<tbody class="divide-y divide-neutral-800">
						{#each data.per_show as sh (sh.id)}
							<tr class="hover:bg-neutral-800/40">
								<td class="px-4 py-2">
									<a
										href="/admin/shows/{sh.id}"
										class="font-medium text-neutral-100 hover:underline"
									>
										{sh.title}
									</a>
									<div class="text-xs text-neutral-500">{sh.slug}</div>
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-emerald-400">
									{sh.sold} / {sh.total}
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-amber-300">
									{sh.held}
								</td>
								<td class="px-4 py-2 text-right tabular-nums text-neutral-300">
									{sh.free}
								</td>
								<td class="px-4 py-2 text-right tabular-nums font-semibold">
									{formatUAH(sh.revenue_kopecks)}
								</td>
							</tr>
						{/each}
					</tbody>
				</table>
			</div>
		{/if}
	</section>

	<p class="mt-4 text-xs text-neutral-500">
		Період: {new Date(data.from).toLocaleDateString('uk-UA')} —
		{new Date(data.to).toLocaleDateString('uk-UA')}. Виторг рахується тільки за
		оплачені замовлення.
	</p>
{/if}
