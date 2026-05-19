<script lang="ts">
	// Placeholder admin dashboard. Real content (events list, seat editor,
	// guest list, broadcast) arrives in PR #4. For now we just confirm the
	// session cookie made the redirect land somewhere sensible.
	//
	// The dashboard isn't gated on the client side yet — backend cookie
	// auth happens at the /api/admin/* endpoints (to be added), and the
	// SPA shell is intentionally servable to anyone. The login form
	// itself sits on the Go side at /admin/login.

	async function logout() {
		// SameSite=Lax cookie + same-origin POST — no CSRF token needed
		// for this single destructive endpoint.
		await fetch('/admin/logout', { method: 'POST', credentials: 'same-origin' });
		// Server returns 303; fetch follows it. After logout the cookie
		// is cleared, so a hard navigation lands on the login form.
		window.location.href = '/admin/login';
	}
</script>

<svelte:head>
	<title>monokasa · admin</title>
</svelte:head>

<main class="mx-auto max-w-3xl p-6">
	<header class="mb-8 flex items-center justify-between">
		<h1 class="text-2xl font-semibold tracking-tight">monokasa · admin</h1>
		<button
			onclick={logout}
			class="rounded-md border border-neutral-700 px-3 py-1.5 text-sm text-neutral-300 hover:bg-neutral-800"
		>
			Вийти
		</button>
	</header>

	<section class="rounded-lg border border-neutral-800 bg-neutral-900 p-6">
		<h2 class="text-lg font-medium">Залогінено ✓</h2>
		<p class="mt-2 text-sm text-neutral-400">
			Це placeholder адмін-панелі. Повноцінний UI прийде наступним PR:
			список подій, редактор зали (drag&amp;drop на canvas), список гостей з
			фільтрами і CSV-експортом, broadcast куплених, кнопки "перевипустити QR"
			та "скасувати бронь".
		</p>
		<p class="mt-2 text-sm text-neutral-500">
			Поточна сесія живе 30 днів, лежить у HttpOnly cookie <code class="rounded bg-neutral-950 px-1 py-0.5 text-xs">monokasa_admin</code>.
		</p>
	</section>

	<section class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900 p-6">
		<h3 class="text-sm font-semibold text-neutral-300">Що вже працює зараз</h3>
		<ul class="mt-3 list-disc pl-5 text-sm text-neutral-400">
			<li>Telegram-бот: <code>/seats</code>, <code>/my</code>, <code>/stats</code>, <code>/reconcile</code>, <code>/jar</code></li>
			<li>monobank webhook на <code>/webhook</code></li>
			<li>Сканер QR на <code>/scan</code></li>
			<li>Reconcile-команда для пропущених webhook'ів</li>
		</ul>
	</section>
</main>
