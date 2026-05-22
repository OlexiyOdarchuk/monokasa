<script lang="ts">
	import { api, type DiscountCode, type DiscountInput, ApiError } from '$lib/api';

	let codes = $state<DiscountCode[]>([]);
	let loaded = $state(false);
	let error = $state('');

	// Create form
	let newCode = $state('');
	let newKind = $state<'percent' | 'fixed'>('percent');
	let newValue = $state(10);
	let newScope = $state<'order' | 'ticket'>('order');
	let newMaxUses = $state(0);
	let newExpires = $state(''); // YYYY-MM-DD or empty
	let creating = $state(false);
	let createError = $state('');

	async function load() {
		try {
			codes = await api.get<DiscountCode[]>('/api/admin/discounts');
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			loaded = true;
		}
	}

	$effect(() => {
		load();
	});

	async function create(e: Event) {
		e.preventDefault();
		creating = true;
		createError = '';
		try {
			let expiresAt: string | null = null;
			if (newExpires) {
				// YYYY-MM-DD → end-of-day ISO so the code is valid through that day.
				expiresAt = new Date(newExpires + 'T23:59:59').toISOString();
			}
			const value =
				newKind === 'percent' ? newValue : Math.round(newValue * 100); // kopecks
			const input: DiscountInput = {
				code: newCode.trim().toUpperCase(),
				kind: newKind,
				value,
				scope: newScope,
				max_uses: newMaxUses,
				expires_at: expiresAt,
				active: true
			};
			await api.post<DiscountCode>('/api/admin/discounts', input);
			newCode = '';
			newValue = newKind === 'percent' ? 10 : 50;
			newMaxUses = 0;
			newExpires = '';
			await load();
		} catch (e) {
			if (e instanceof ApiError) createError = e.detail || e.code;
			else createError = String(e);
		} finally {
			creating = false;
		}
	}

	async function toggleActive(c: DiscountCode) {
		try {
			await api.patch(`/api/admin/discounts/${c.id}`, {
				code: c.code,
				kind: c.kind,
				value: c.value,
				scope: c.scope,
				max_uses: c.max_uses,
				expires_at: c.expires_at ?? null,
				active: !c.active
			});
			await load();
		} catch (e) {
			error = e instanceof ApiError ? e.detail || e.code : String(e);
		}
	}

	async function remove(c: DiscountCode) {
		if (!confirm(`Видалити промокод ${c.code}?`)) return;
		try {
			await api.del(`/api/admin/discounts/${c.id}`);
			await load();
		} catch (e) {
			error = e instanceof ApiError ? e.detail || e.code : String(e);
		}
	}

	function formatValue(c: DiscountCode): string {
		const base = c.kind === 'percent' ? `−${c.value}%` : `−${(c.value / 100).toLocaleString('uk-UA')} ₴`;
		return c.scope === 'ticket' ? `${base} / квиток` : base;
	}

	function formatExpires(iso?: string | null): string {
		if (!iso) return '∞';
		return new Date(iso).toLocaleDateString('uk-UA');
	}
</script>

<svelte:head>
	<title>monokasa · промокоди</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← Назад</a>
</div>
<h1 class="mt-2 text-2xl font-semibold tracking-tight">Промокоди</h1>
<p class="mt-1 text-sm text-neutral-400">
	Знижки, які покупець вводить на checkout. Працюють для оплат через сайт.
</p>

<!-- Create form -->
<form onsubmit={create} class="mt-6 grid grid-cols-1 gap-3 rounded-lg border border-neutral-800 bg-neutral-900 p-4 sm:grid-cols-[1fr_auto_auto_auto_auto_auto_auto]">
	<div>
		<label for="dcode" class="block text-xs text-neutral-400">Код</label>
		<input
			id="dcode"
			type="text"
			bind:value={newCode}
			required
			minlength="2"
			maxlength="40"
			placeholder="EARLYBIRD"
			class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-1.5 text-sm uppercase"
		/>
	</div>
	<div>
		<label for="dkind" class="block text-xs text-neutral-400">Тип</label>
		<select
			id="dkind"
			bind:value={newKind}
			class="mt-1 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-sm"
		>
			<option value="percent">%</option>
			<option value="fixed">₴</option>
		</select>
	</div>
	<div>
		<label for="dval" class="block text-xs text-neutral-400">
			{newKind === 'percent' ? 'Відсоток' : 'Сума (₴)'}
		</label>
		<input
			id="dval"
			type="number"
			min="1"
			max={newKind === 'percent' ? 100 : 999999}
			step="1"
			bind:value={newValue}
			required
			class="mt-1 w-24 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-right text-sm"
		/>
	</div>
	<div>
		<label for="dscope" class="block text-xs text-neutral-400">На що</label>
		<select
			id="dscope"
			bind:value={newScope}
			title={newScope === 'order'
				? 'Знижка застосовується до суми всього замовлення'
				: 'Знижка обмежена ціною одного квитка — для одиничних компів'}
			class="mt-1 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-sm"
		>
			<option value="order">кошик</option>
			<option value="ticket">квиток</option>
		</select>
	</div>
	<div>
		<label for="dmax" class="block text-xs text-neutral-400">Макс. використань</label>
		<input
			id="dmax"
			type="number"
			min="0"
			step="1"
			bind:value={newMaxUses}
			class="mt-1 w-24 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-right text-sm"
			placeholder="0=∞"
		/>
	</div>
	<div>
		<label for="dexp" class="block text-xs text-neutral-400">До дати</label>
		<input
			id="dexp"
			type="date"
			bind:value={newExpires}
			class="mt-1 rounded-md border border-neutral-800 bg-neutral-950 px-2 py-1.5 text-sm"
		/>
	</div>
	<div class="flex items-end">
		<button
			type="submit"
			disabled={creating}
			class="rounded-md bg-[var(--color-brand)] px-4 py-1.5 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
		>
			+ Додати
		</button>
	</div>
</form>
{#if createError}
	<p class="mt-2 text-sm text-red-300">{createError}</p>
{/if}

<!-- List -->
{#if error}
	<div class="mt-6 rounded-md border border-red-900 bg-red-950/50 p-4 text-sm text-red-300">
		{error}
	</div>
{:else if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else if codes.length === 0}
	<p class="mt-6 text-sm text-neutral-500">Поки що нема промокодів.</p>
{:else}
	<div class="mt-6 overflow-x-auto rounded-lg border border-neutral-800 bg-neutral-900">
		<table class="w-full text-sm">
			<thead class="text-xs uppercase text-neutral-500">
				<tr>
					<th class="px-4 py-2 text-left">Код</th>
					<th class="px-4 py-2 text-right">Знижка</th>
					<th class="px-4 py-2 text-center">На що</th>
					<th class="px-4 py-2 text-right">Використано</th>
					<th class="px-4 py-2 text-right">Діє до</th>
					<th class="px-4 py-2 text-center">Активний</th>
					<th class="px-4 py-2"></th>
				</tr>
			</thead>
			<tbody class="divide-y divide-neutral-800">
				{#each codes as c (c.id)}
					<tr class="hover:bg-neutral-800/40">
						<td class="px-4 py-2 font-mono font-medium">{c.code}</td>
						<td class="px-4 py-2 text-right font-semibold tabular-nums text-emerald-400">
							{formatValue(c)}
						</td>
						<td class="px-4 py-2 text-center text-xs text-neutral-400">
							{c.scope === 'ticket' ? 'квиток' : 'кошик'}
						</td>
						<td class="px-4 py-2 text-right tabular-nums">
							{c.used_count}{c.max_uses > 0 ? ` / ${c.max_uses}` : ''}
						</td>
						<td class="px-4 py-2 text-right tabular-nums">{formatExpires(c.expires_at)}</td>
						<td class="px-4 py-2 text-center">
							<button
								type="button"
								onclick={() => toggleActive(c)}
								class="text-lg"
								title={c.active ? 'Вимкнути' : 'Увімкнути'}
								aria-label={c.active ? 'Вимкнути' : 'Увімкнути'}
							>
								{c.active ? '🟢' : '⚪️'}
							</button>
						</td>
						<td class="px-4 py-2 text-right">
							<button
								type="button"
								onclick={() => remove(c)}
								class="rounded px-2 py-1 text-xs text-red-400 hover:bg-red-950/40"
								aria-label="Видалити {c.code}"
							>
								Видалити
							</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
