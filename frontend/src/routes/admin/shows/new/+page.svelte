<script lang="ts">
	import { goto } from '$app/navigation';
	import { api, type Show, type CreateShowInput, type ShowKind, ApiError } from '$lib/api';
	import DateTimePicker from '$lib/DateTimePicker.svelte';

	let title = $state('');
	let venue = $state('');
	let startsAtISO = $state(''); // RFC3339 UTC, populated by DateTimePicker
	let kind = $state<ShowKind>('seated');
	let rows = $state(5);
	let cols = $state(6);
	let gaCapacity = $state(100);
	let priceUAH = $state('250');

	let submitting = $state(false);
	let error = $state('');

	async function submit(e: Event) {
		e.preventDefault();
		error = '';

		const priceKopecks = Math.round(parseFloat(priceUAH.replace(',', '.')) * 100);
		if (!Number.isFinite(priceKopecks) || priceKopecks < 0) {
			error = 'Ціна виглядає дивно — введи число у гривнях, напр. 250.00';
			return;
		}
		if (!startsAtISO) {
			error = 'Заповни дату та час початку';
			return;
		}

		const input: CreateShowInput = {
			title: title.trim(),
			venue: venue.trim(),
			starts_at: startsAtISO,
			rows: kind === 'seated' ? rows : 0,
			cols: kind === 'seated' ? cols : 0,
			price_kopecks: priceKopecks,
			kind,
			ga_capacity: kind === 'ga' ? gaCapacity : 0
		};

		submitting = true;
		try {
			const show = await api.post<Show>('/api/admin/shows', input);
			await goto(`/admin/shows/${show.id}`);
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			submitting = false;
		}
	}
</script>

<svelte:head>
	<title>monokasa · нова подія</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← Назад</a>
</div>
<h1 class="mt-2 text-2xl font-semibold tracking-tight">Нова подія</h1>

<form onsubmit={submit} class="mt-6 max-w-xl space-y-5">
	<div>
		<label for="title" class="block text-sm text-neutral-400">Назва події</label>
		<input
			id="title"
			type="text"
			bind:value={title}
			required
			minlength="2"
			class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
		/>
	</div>

	<div>
		<label for="venue" class="block text-sm text-neutral-400">Місце проведення</label>
		<input
			id="venue"
			type="text"
			bind:value={venue}
			class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
		/>
	</div>

	<div>
		<label for="startsAt" class="block text-sm text-neutral-400">Початок (твоя локальна зона)</label>
		<div class="mt-1">
			<DateTimePicker bind:value={startsAtISO} id="startsAt" required />
		</div>
	</div>

	<div>
		<span class="block text-sm text-neutral-400">Тип події</span>
		<div class="mt-2 grid grid-cols-1 gap-2 sm:grid-cols-2">
			<label
				class="flex cursor-pointer items-start gap-3 rounded-md border border-neutral-800 bg-neutral-900 p-3 hover:border-neutral-600 {kind ===
				'seated'
					? 'ring-2 ring-[var(--color-brand)]'
					: ''}"
			>
				<input type="radio" bind:group={kind} value="seated" class="mt-1 accent-[var(--color-brand)]" />
				<span class="flex-1 text-sm">
					<span class="font-medium text-neutral-100">🎭 Сидячі місця</span>
					<span class="mt-0.5 block text-xs text-neutral-500">
						Сітка з рядами і кріслами. Покупець обирає конкретне місце.
					</span>
				</span>
			</label>
			<label
				class="flex cursor-pointer items-start gap-3 rounded-md border border-neutral-800 bg-neutral-900 p-3 hover:border-neutral-600 {kind ===
				'ga'
					? 'ring-2 ring-[var(--color-brand)]'
					: ''}"
			>
				<input type="radio" bind:group={kind} value="ga" class="mt-1 accent-[var(--color-brand)]" />
				<span class="flex-1 text-sm">
					<span class="font-medium text-neutral-100">🎤 Загальний вхід (GA)</span>
					<span class="mt-0.5 block text-xs text-neutral-500">
						Без сидячих місць — тільки кількість. Для standup, лекцій, клубів.
					</span>
				</span>
			</label>
		</div>
	</div>

	{#if kind === 'seated'}
		<div class="grid grid-cols-2 gap-4">
			<div>
				<label for="rows" class="block text-sm text-neutral-400">Рядів</label>
				<input
					id="rows"
					type="number"
					min="1"
					max="100"
					bind:value={rows}
					required
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
			<div>
				<label for="cols" class="block text-sm text-neutral-400">Місць у ряді</label>
				<input
					id="cols"
					type="number"
					min="1"
					max="100"
					bind:value={cols}
					required
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
		</div>
	{:else}
		<div>
			<label for="gaCapacity" class="block text-sm text-neutral-400">Скільки квитків усього</label>
			<input
				id="gaCapacity"
				type="number"
				min="1"
				max="5000"
				bind:value={gaCapacity}
				required
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
			/>
			<p class="mt-1 text-xs text-neutral-500">
				Скільки людей зможе купити квиток. Покупець обирає кількість, місця не фіксовані.
			</p>
		</div>
	{/if}

	<div>
		<label for="price" class="block text-sm text-neutral-400">Ціна за місце (₴)</label>
		<input
			id="price"
			type="text"
			inputmode="decimal"
			bind:value={priceUAH}
			required
			class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 text-neutral-100 focus:border-neutral-600 focus:outline-none"
		/>
		<p class="mt-1 text-xs text-neutral-500">
			Однакова ціна для всіх місць; категорії з різною ціною — у редакторі залу (наступний PR).
		</p>
	</div>

	{#if error}
		<div class="rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
			{error}
		</div>
	{/if}

	<div class="flex justify-end gap-3">
		<a
			href="/admin"
			class="rounded-md border border-neutral-700 px-4 py-2 text-sm text-neutral-300 hover:bg-neutral-800"
		>
			Скасувати
		</a>
		<button
			type="submit"
			disabled={submitting}
			class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
		>
			{submitting ? 'Створюю…' : 'Створити'}
		</button>
	</div>
</form>
