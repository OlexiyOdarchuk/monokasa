<script lang="ts">
	import { page } from '$app/state';
	import { api, type Seat, type Show, ApiError } from '$lib/api';

	const showId = $derived(Number(page.params.id));

	let show = $state<Show | null>(null);
	let seats = $state<Seat[]>([]);
	let loaded = $state(false);
	let error = $state('');

	// Map of seat id → "dirty" flag. When non-empty, "Save" is enabled.
	// Add and remove operations hit the API immediately (no need to track
	// them here); only x/y/label/category/price/sellable batch through.
	let dirty = $state<Set<number>>(new Set());

	let selectedId = $state<number | null>(null);
	let saving = $state(false);
	let savedAt = $state<Date | null>(null);

	// Drag state. seatId of the seat being moved, pointer offset so the
	// cursor stays anchored to the spot the user grabbed.
	let draggingId = $state<number | null>(null);
	let dragOffsetX = 0;
	let dragOffsetY = 0;
	let svgEl: SVGSVGElement | null = $state(null);

	const SEAT_R = 22; // radius for the seat circle in SVG units
	const PAD = 60;

	// Category palette — fixed for well-known names, hashed otherwise so
	// admins can invent new categories on the fly and still get a stable
	// colour. Non-sellable always overrides with grey.
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
		// Deterministic hash → hue. Saturation/lightness fixed for legibility.
		let h = 0;
		for (let i = 0; i < k.length; i++) h = (h * 31 + k.charCodeAt(i)) >>> 0;
		return `hsl(${h % 360}, 65%, 55%)`;
	}

	const selected = $derived(seats.find((s) => s.id === selectedId) ?? null);

	const viewBox = $derived.by(() => {
		if (seats.length === 0) return '0 0 600 400';
		let maxX = 0;
		let maxY = 0;
		for (const s of seats) {
			if (s.x > maxX) maxX = s.x;
			if (s.y > maxY) maxY = s.y;
		}
		return `0 0 ${maxX + PAD} ${maxY + PAD + 40}`; // +40 reserves room for stage label
	});

	async function load() {
		try {
			const [showData, seatData] = await Promise.all([
				api.get<Show>(`/api/admin/shows/${showId}`),
				api.get<Seat[]>(`/api/admin/shows/${showId}/seats`)
			]);
			show = showData;
			seats = seatData;
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

	function markDirty(id: number) {
		dirty.add(id);
		dirty = dirty; // notify Svelte 5 of mutation
	}

	function patchSeat(id: number, patch: Partial<Seat>) {
		seats = seats.map((s) => (s.id === id ? { ...s, ...patch } : s));
		markDirty(id);
	}

	// Convert client (mouse) coordinates into SVG-user coordinates,
	// taking viewBox and any CSS transform into account. Without the CTM
	// the math drifts as soon as the SVG is scaled by its container.
	function clientToSVG(clientX: number, clientY: number): { x: number; y: number } {
		if (!svgEl) return { x: 0, y: 0 };
		const pt = svgEl.createSVGPoint();
		pt.x = clientX;
		pt.y = clientY;
		const ctm = svgEl.getScreenCTM();
		if (!ctm) return { x: 0, y: 0 };
		const inv = ctm.inverse();
		const local = pt.matrixTransform(inv);
		return { x: local.x, y: local.y };
	}

	function onSeatPointerDown(seat: Seat, e: PointerEvent) {
		// Left click only — ignore right-clicks, middle, touch-extra.
		if (e.button !== 0 && e.pointerType === 'mouse') return;
		e.preventDefault();
		selectedId = seat.id;
		draggingId = seat.id;
		const local = clientToSVG(e.clientX, e.clientY);
		dragOffsetX = local.x - seat.x;
		dragOffsetY = local.y - seat.y;
		(e.target as Element).setPointerCapture(e.pointerId);
	}

	function onSeatPointerMove(e: PointerEvent) {
		if (draggingId === null) return;
		const local = clientToSVG(e.clientX, e.clientY);
		const nx = Math.max(SEAT_R, local.x - dragOffsetX);
		const ny = Math.max(SEAT_R, local.y - dragOffsetY);
		patchSeat(draggingId, { x: nx, y: ny });
	}

	function onSeatPointerUp(e: PointerEvent) {
		if (draggingId === null) return;
		(e.target as Element).releasePointerCapture?.(e.pointerId);
		draggingId = null;
	}

	// Sidebar edits — same flow, just driven by inputs instead of drag.
	function updateLabel(v: string) {
		if (selected) patchSeat(selected.id, { label: v });
	}
	function updateCategory(v: string) {
		if (selected) patchSeat(selected.id, { category: v });
	}
	function updatePrice(v: string) {
		if (!selected) return;
		const n = Math.round(parseFloat(v.replace(',', '.')) * 100);
		if (!Number.isFinite(n) || n < 0) return;
		patchSeat(selected.id, { price_kopecks: n });
	}
	function toggleSellable() {
		if (selected) patchSeat(selected.id, { sellable: !selected.sellable });
	}

	async function save() {
		if (dirty.size === 0) return;
		const patches = seats
			.filter((s) => dirty.has(s.id))
			.map((s) => ({
				id: s.id,
				x: s.x,
				y: s.y,
				label: s.label,
				category: s.category,
				price_kopecks: s.price_kopecks,
				sellable: s.sellable
			}));
		saving = true;
		error = '';
		try {
			await api.patch('/api/admin/seats', patches);
			dirty = new Set();
			savedAt = new Date();
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			saving = false;
		}
	}

	// --- add seat dialog ---
	let showAdd = $state(false);
	let addRow = $state(1);
	let addCol = $state(1);
	let addPrice = $state('250');
	let adding = $state(false);
	let addError = $state('');

	function openAdd() {
		// Default to first slot past the highest row to avoid the obvious collision.
		let maxRow = 0;
		for (const s of seats) if (s.row > maxRow) maxRow = s.row;
		addRow = maxRow + 1;
		addCol = 1;
		addError = '';
		showAdd = true;
	}

	async function submitAdd(e: Event) {
		e.preventDefault();
		const pk = Math.round(parseFloat(addPrice.replace(',', '.')) * 100);
		if (!Number.isFinite(pk) || pk < 0) {
			addError = 'Невірна ціна';
			return;
		}
		adding = true;
		addError = '';
		try {
			const seat = await api.post<Seat>(`/api/admin/shows/${showId}/seats`, {
				row: addRow,
				col: addCol,
				x: (addCol - 1) * 100 + 50,
				y: (addRow - 1) * 100 + 50,
				label: '',
				category: '',
				price_kopecks: pk,
				sellable: true
			});
			seats = [...seats, seat];
			selectedId = seat.id;
			showAdd = false;
		} catch (e) {
			if (e instanceof ApiError) addError = e.detail || e.code;
			else addError = String(e);
		} finally {
			adding = false;
		}
	}

	async function removeSelected() {
		if (!selected) return;
		if (!confirm(`Видалити місце ряд ${selected.row} · ${selected.col}?`)) return;
		try {
			await api.del(`/api/admin/seats/${selected.id}`);
			seats = seats.filter((s) => s.id !== selected!.id);
			dirty.delete(selected.id);
			dirty = dirty;
			selectedId = null;
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		}
	}

	function formatUAH(k: number): string {
		return (k / 100).toLocaleString('uk-UA', { minimumFractionDigits: 2 });
	}
</script>

<svelte:head>
	<title>monokasa · редактор залу</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin/shows/{showId}" class="text-sm text-neutral-400 hover:text-neutral-200"
		>← До події</a
	>
</div>

<div class="mt-2 flex flex-wrap items-baseline justify-between gap-3">
	<h1 class="text-2xl font-semibold tracking-tight">
		Редактор залу{#if show}<span class="ml-2 text-neutral-500">· {show.title}</span>{/if}
	</h1>
	<div class="flex items-center gap-3">
		{#if savedAt && dirty.size === 0}
			<span class="text-xs text-neutral-500">Збережено о {savedAt.toLocaleTimeString('uk-UA')}</span>
		{/if}
		<button
			onclick={openAdd}
			class="rounded-md border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
		>
			+ Додати місце
		</button>
		<button
			onclick={save}
			disabled={saving || dirty.size === 0}
			class="rounded-md bg-[var(--color-brand)] px-4 py-1.5 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
		>
			{#if saving}Зберігаю…{:else if dirty.size > 0}Зберегти ({dirty.size}){:else}Збережено{/if}
		</button>
	</div>
</div>

{#if error}
	<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
		{error}
	</div>
{/if}

{#if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else}
	<div class="mt-4 flex gap-4">
		<!-- canvas -->
		<div class="flex-1 overflow-auto rounded-lg border border-neutral-800 bg-neutral-950 p-2">
			<svg
				bind:this={svgEl}
				viewBox={viewBox}
				class="block max-h-[70vh] w-full select-none"
				role="application"
				aria-label="Редактор розташування місць"
				onpointermove={onSeatPointerMove}
				onpointerup={onSeatPointerUp}
			>
				<!-- stage label at top -->
				<rect x="0" y="0" width="100%" height="32" fill="#1a1a1a" />
				<text x="50%" y="20" text-anchor="middle" fill="#9ca3af" font-size="14" font-family="system-ui">
					━━━━━ СЦЕНА ━━━━━
				</text>

				<!-- seats -->
				{#each seats as seat (seat.id)}
					{@const isSel = selectedId === seat.id}
					{@const fill = seat.sellable ? categoryColor(seat.category) : '#4b5563'}
					<g
						transform="translate({seat.x} {seat.y + 40})"
						class="cursor-grab"
						role="button"
						tabindex="0"
						aria-label="Місце ряд {seat.row} місце {seat.col}"
						onpointerdown={(e: PointerEvent) => onSeatPointerDown(seat, e)}
					>
						<circle
							r={SEAT_R}
							fill={fill}
							fill-opacity={seat.sellable ? 0.85 : 0.4}
							stroke={isSel ? '#ffffff' : '#0a0a0a'}
							stroke-width={isSel ? 3 : 1}
						/>
						<text
							y="5"
							text-anchor="middle"
							fill="#fff"
							font-size="11"
							font-weight="600"
							font-family="system-ui"
							pointer-events="none"
						>
							{seat.label || `${seat.row}-${seat.col}`}
						</text>
						{#if !seat.sellable}
							<line
								x1={-SEAT_R}
								y1={-SEAT_R}
								x2={SEAT_R}
								y2={SEAT_R}
								stroke="#1a1a1a"
								stroke-width="2"
								pointer-events="none"
							/>
						{/if}
					</g>
				{/each}
			</svg>
		</div>

		<!-- sidebar -->
		<aside class="w-72 shrink-0 rounded-lg border border-neutral-800 bg-neutral-900 p-4">
			{#if selected}
				<h2 class="text-sm font-medium text-neutral-300">
					Ряд {selected.row} · місце {selected.col}
				</h2>
				<dl class="mt-2 text-xs text-neutral-500">
					<dt class="inline">ID:</dt>
					<dd class="inline">{selected.id}</dd>
				</dl>

				<div class="mt-4 space-y-3">
					<div>
						<label for="sl" class="block text-xs text-neutral-400">Підпис</label>
						<input
							id="sl"
							type="text"
							value={selected.label}
							oninput={(e: Event) => updateLabel((e.target as HTMLInputElement).value)}
							placeholder="напр. A1, VIP-3"
							class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm focus:border-neutral-600 focus:outline-none"
						/>
					</div>
					<div>
						<label for="sc" class="block text-xs text-neutral-400">Категорія</label>
						<input
							id="sc"
							type="text"
							value={selected.category}
							oninput={(e: Event) => updateCategory((e.target as HTMLInputElement).value)}
							placeholder="vip / standard / balcony…"
							class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm focus:border-neutral-600 focus:outline-none"
						/>
					</div>
					<div>
						<label for="sp" class="block text-xs text-neutral-400">Ціна (₴)</label>
						<input
							id="sp"
							type="text"
							inputmode="decimal"
							value={formatUAH(selected.price_kopecks)}
							onblur={(e: Event) => updatePrice((e.target as HTMLInputElement).value)}
							class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm focus:border-neutral-600 focus:outline-none"
						/>
					</div>
					<div>
						<label class="flex items-center gap-2 text-sm text-neutral-300">
							<input
								type="checkbox"
								checked={selected.sellable}
								onchange={toggleSellable}
								class="size-4 accent-[var(--color-brand)]"
							/>
							Продається (інакше — прохід/тех. місце)
						</label>
					</div>

					<button
						onclick={removeSelected}
						class="mt-4 w-full rounded border border-red-900 bg-red-950/30 px-3 py-1.5 text-sm text-red-300 hover:bg-red-950/60"
					>
						Видалити місце
					</button>
					<p class="text-xs text-neutral-500">
						Видалити можна лише місця без жодних бронювань (навіть скасованих).
					</p>
				</div>
			{:else}
				<p class="text-sm text-neutral-500">
					Тицьни місце на канві, щоб редагувати. Потягни — щоб перемістити.
				</p>
				<p class="mt-3 text-xs text-neutral-600">
					Кольори зчитуються з категорії; невідомі категорії отримують стабільний
					колір через hash. Сірі перекреслені — non-sellable (проходи).
				</p>
			{/if}
		</aside>
	</div>
{/if}

<!-- add seat modal -->
{#if showAdd}
	<div
		class="fixed inset-0 z-40 grid place-items-center bg-black/60 p-4"
		onclick={(e: MouseEvent) => {
			if (e.target === e.currentTarget) showAdd = false;
		}}
		role="presentation"
	>
		<form
			onsubmit={submitAdd}
			class="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-5"
		>
			<h2 class="text-lg font-medium">Нове місце</h2>
			<div class="mt-4 grid grid-cols-2 gap-3">
				<div>
					<label for="ar" class="block text-xs text-neutral-400">Ряд</label>
					<input
						id="ar"
						type="number"
						min="1"
						bind:value={addRow}
						required
						class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm"
					/>
				</div>
				<div>
					<label for="ac" class="block text-xs text-neutral-400">Місце</label>
					<input
						id="ac"
						type="number"
						min="1"
						bind:value={addCol}
						required
						class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm"
					/>
				</div>
			</div>
			<div class="mt-3">
				<label for="ap" class="block text-xs text-neutral-400">Ціна (₴)</label>
				<input
					id="ap"
					type="text"
					inputmode="decimal"
					bind:value={addPrice}
					required
					class="mt-1 w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-sm"
				/>
			</div>
			{#if addError}
				<div class="mt-3 rounded border border-red-900 bg-red-950/50 p-2 text-xs text-red-300">
					{addError}
				</div>
			{/if}
			<div class="mt-5 flex justify-end gap-2">
				<button
					type="button"
					onclick={() => (showAdd = false)}
					class="rounded border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
				>
					Скасувати
				</button>
				<button
					type="submit"
					disabled={adding}
					class="rounded bg-[var(--color-brand)] px-3 py-1.5 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
				>
					{adding ? 'Додаю…' : 'Додати'}
				</button>
			</div>
		</form>
	</div>
{/if}
