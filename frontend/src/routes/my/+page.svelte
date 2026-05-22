<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/state';
	import QRCode from 'qrcode';
	import { publicApi, type BuyerTicket, ApiError } from '$lib/api';

	type Mode = 'loading' | 'login' | 'sent' | 'sentDev' | 'tokenError' | 'tickets';
	let mode = $state<Mode>('loading');
	let email = $state('');
	let myEmail = $state('');
	let tickets = $state<BuyerTicket[]>([]);
	let error = $state('');
	let submitting = $state(false);
	let tokenErrorMsg = $state('');

	// Magic-link clicks hit /api/public/login/consume first; that
	// server-side handler sets the cookie and 303s here. On error it
	// 303s here with ?error=… so we can show a friendly message
	// instead of raw JSON in the address bar.
	onMount(async () => {
		const err = page.url.searchParams.get('error');
		if (err) {
			// Strip the error param so a refresh doesn't keep showing it.
			const url = new URL(window.location.href);
			url.searchParams.delete('error');
			window.history.replaceState({}, '', url.toString());
			mode = 'tokenError';
			tokenErrorMsg =
				err === 'expired_token'
					? 'Посилання прострочене — запроси нове.'
					: err === 'invalid_token'
						? 'Посилання застаріле або вже використане — запроси нове.'
						: err === 'missing_token'
							? 'У посиланні немає токена.'
							: 'Не вдалось залогінити, спробуй ще раз.';
			return;
		}
		await refresh();
	});

	async function refresh() {
		try {
			const me = await publicApi.get<{ email: string }>('/api/public/my');
			myEmail = me.email;
			tickets = await publicApi.get<BuyerTicket[]>('/api/public/my/tickets');
			mode = 'tickets';
		} catch (e) {
			if (e instanceof ApiError && e.status === 401) {
				mode = 'login';
				return;
			}
			error = e instanceof ApiError ? e.detail || e.code : String(e);
			mode = 'login';
		}
	}

	async function requestLogin(e: Event) {
		e.preventDefault();
		submitting = true;
		error = '';
		try {
			const r = await publicApi.post<{ status: string }>('/api/public/login/request', {
				email: email.trim()
			});
			// Server returns status: "logged" when SMTP isn't configured —
			// the link went to server logs instead of email. Useful for
			// local dev so the operator can copy it manually.
			mode = r.status === 'logged' ? 'sentDev' : 'sent';
		} catch (err) {
			if (err instanceof ApiError) error = err.detail || err.code;
			else error = String(err);
		} finally {
			submitting = false;
		}
	}

	async function logout() {
		await fetch('/api/public/login/logout', {
			method: 'POST',
			credentials: 'same-origin'
		});
		myEmail = '';
		tickets = [];
		email = '';
		mode = 'login';
	}

	// QR canvas refs are populated as items render. Drawing happens once
	// per refresh — QRCode.toCanvas is sync once the lib is loaded.
	function attachQR(node: HTMLCanvasElement, payload: string) {
		if (!payload) return;
		QRCode.toCanvas(node, payload, { width: 220, margin: 1 }, (err) => {
			if (err) console.warn('qr render', err);
		});
		return {
			update(newPayload: string) {
				if (!newPayload) return;
				QRCode.toCanvas(node, newPayload, { width: 220, margin: 1 }, () => {});
			}
		};
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

	const orderStatusLabels: Record<BuyerTicket['order_status'], { label: string; cls: string }> = {
		paid: { label: 'оплачено', cls: 'bg-emerald-950/50 text-emerald-300 border-emerald-900' },
		held: { label: 'чекає оплати', cls: 'bg-amber-950/50 text-amber-300 border-amber-900' },
		expired: { label: 'термін минув', cls: 'bg-neutral-800 text-neutral-400 border-neutral-700' },
		cancelled: { label: 'скасовано', cls: 'bg-red-950/50 text-red-300 border-red-900' }
	};
</script>

<svelte:head>
	<title>Мої квитки — monokasa</title>
</svelte:head>

<main class="mx-auto max-w-2xl p-4 sm:p-6">
	<header class="mb-6 flex items-center justify-between">
		<h1 class="text-2xl font-semibold tracking-tight">Мої квитки</h1>
		{#if mode === 'tickets'}
			<button
				onclick={logout}
				class="text-sm text-neutral-400 hover:text-neutral-200"
			>
				Вийти
			</button>
		{/if}
	</header>

	{#if mode === 'loading'}
		<div class="text-center text-neutral-500">Перевіряю…</div>
	{:else if mode === 'tokenError'}
		<div class="rounded-2xl border border-red-900 bg-red-950/40 p-6 text-center">
			<div class="text-3xl">⏰</div>
			<h2 class="mt-2 text-lg font-medium">Посилання не спрацювало</h2>
			<p class="mt-2 text-sm text-neutral-300">{tokenErrorMsg}</p>
			<button
				onclick={() => {
					mode = 'login';
					tokenErrorMsg = '';
				}}
				class="mt-4 rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)]"
			>
				Спробувати ще раз
			</button>
		</div>
	{:else if mode === 'sent'}
		<div class="rounded-2xl border border-emerald-900 bg-emerald-950/40 p-6 text-center">
			<div class="text-3xl">📧</div>
			<h2 class="mt-2 text-lg font-medium">Лист пішов</h2>
			<p class="mt-2 text-sm text-neutral-300">
				Перевір <b>{email}</b> — там посилання, що відкриє цю сторінку
				з твоїми квитками. Живе 15 хвилин.
			</p>
			<p class="mt-2 text-xs text-neutral-500">
				Не прийшло? Глянь у "Спам" або запроси ще раз через хвилину.
			</p>
		</div>
	{:else if mode === 'sentDev'}
		<div class="rounded-2xl border border-amber-900 bg-amber-950/40 p-6 text-center">
			<div class="text-3xl">⚠️</div>
			<h2 class="mt-2 text-lg font-medium">SMTP не налаштований</h2>
			<p class="mt-2 text-sm text-neutral-300">
				Magic-link <b>не</b> пішов поштою — він написаний у логах
				сервера. Шукай рядок <code class="rounded bg-neutral-900 px-1.5 py-0.5 font-mono text-xs">magic link printed in logs</code>
				і скопіюй URL звідти.
			</p>
			<p class="mt-3 text-xs text-neutral-500">
				У проді обов'язково підняти SMTP — інакше будь-хто з доступом
				до логів зможе залогінитись за чужий email.
			</p>
		</div>
	{:else if mode === 'login'}
		<form
			onsubmit={requestLogin}
			class="rounded-2xl border border-neutral-800 bg-neutral-900 p-6"
		>
			<p class="mb-4 text-sm text-neutral-300">
				Введи email, на який купував квитки. Ми надішлемо одноразове
				посилання — клік і ти бачиш усі свої бронювання.
			</p>
			<label for="email" class="block text-sm text-neutral-400">Email</label>
			<input
				id="email"
				type="email"
				bind:value={email}
				required
				placeholder="email@example.com"
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 text-base focus:border-neutral-600 focus:outline-none"
			/>
			{#if error}
				<div class="mt-3 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
					{error}
				</div>
			{/if}
			<button
				type="submit"
				disabled={submitting}
				class="mt-4 w-full rounded-md bg-[var(--color-brand)] px-4 py-2 text-base font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
			>
				{submitting ? 'Надсилаю…' : 'Надіслати посилання'}
			</button>
		</form>
	{:else if mode === 'tickets'}
		<p class="mb-4 text-xs text-neutral-500">Email: {myEmail}</p>
		{#if tickets.length === 0}
			<div class="rounded-2xl border border-neutral-800 bg-neutral-900 p-6 text-center">
				<p class="text-neutral-400">У тебе ще немає бронювань на цей email.</p>
				<a
					href="/"
					class="mt-3 inline-block text-sm text-[var(--color-brand)] hover:underline"
				>
					← До афіші
				</a>
			</div>
		{:else}
			<div class="space-y-6">
				{#each tickets as order (order.order_id)}
					{@const style = orderStatusLabels[order.order_status]}
					<section class="overflow-hidden rounded-2xl border border-neutral-800 bg-neutral-900">
						<header class="border-b border-neutral-800 p-5">
							<div class="flex items-baseline justify-between gap-3">
								<h2 class="text-lg font-semibold">{order.show.title}</h2>
								<span class="shrink-0 rounded border px-2 py-0.5 text-xs {style.cls}">
									{style.label}
								</span>
							</div>
							<p class="mt-1 text-sm text-neutral-400">{startsAtText(order.show.starts_at)}</p>
							{#if order.show.venue}
								<p class="text-sm text-neutral-400">📍 {order.show.venue}</p>
							{/if}
							<p class="mt-2 text-xs text-neutral-500">
								Код: <span class="font-mono">{order.order_code}</span> · сума {formatUAH(order.total_kopecks)}
							</p>
						</header>
						<ul class="divide-y divide-neutral-900">
							{#each order.items as it (it.reservation_id)}
								<li class="flex items-start gap-4 p-5">
									{#if order.order_status === 'paid' && it.qr_payload && !it.cancelled_at}
										<canvas
											use:attachQR={it.qr_payload}
											class="shrink-0 rounded bg-white p-2"
											aria-label="QR-код квитка"
										></canvas>
									{:else}
										<div class="grid h-[220px] w-[220px] shrink-0 place-items-center rounded border border-dashed border-neutral-800 bg-neutral-950 text-xs text-neutral-500">
											{#if it.cancelled_at}
												скасовано
											{:else if order.order_status === 'held'}
												QR з'явиться після оплати
											{:else}
												—
											{/if}
										</div>
									{/if}
									<div class="min-w-0 flex-1">
										<div class="text-base font-medium">
											ряд {it.row} · місце {it.col}{it.label ? ` · ${it.label}` : ''}
										</div>
										{#if it.attendee_name}
											<div class="text-sm text-neutral-400">На ім'я: {it.attendee_name}</div>
										{/if}
										<div class="mt-1 text-sm text-neutral-500">{formatUAH(it.price_kopecks)}</div>
										<div class="mt-2 flex flex-wrap gap-1">
											{#if it.used_at}
												<span class="rounded border border-neutral-700 bg-neutral-800 px-2 py-0.5 text-xs text-neutral-400">
													вже сканувався
												</span>
											{/if}
											{#if it.cancelled_at}
												<span class="rounded border border-red-900 bg-red-950/50 px-2 py-0.5 text-xs text-red-300">
													скасовано
												</span>
											{/if}
											{#if it.refunded_at}
												<span class="rounded border border-violet-900 bg-violet-950/50 px-2 py-0.5 text-xs text-violet-300">
													повернуто
												</span>
											{/if}
										</div>
									</div>
								</li>
							{/each}
						</ul>
					</section>
				{/each}
			</div>
		{/if}
	{/if}
</main>
