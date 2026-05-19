<script lang="ts">
	import { api, type Me, ApiError } from '$lib/api';

	let { children } = $props();

	let me = $state<Me | null>(null);
	let loaded = $state(false);

	// Auth-check on mount. /api/admin/me through api.get auto-redirects on
	// 401, so success here means we have a valid session.
	$effect(() => {
		(async () => {
			try {
				me = await api.get<Me>('/api/admin/me');
			} catch (e) {
				// 401 was already handled inside api.ts (window.location).
				// Other errors land here — surface to console so the dev
				// notices, but don't block the shell from rendering.
				if (!(e instanceof ApiError) || e.status !== 401) {
					console.error('auth check failed', e);
				}
			} finally {
				loaded = true;
			}
		})();
	});

	async function logout() {
		await fetch('/admin/logout', { method: 'POST', credentials: 'same-origin' });
		window.location.href = '/admin/login';
	}
</script>

<div class="min-h-screen">
	<header class="border-b border-neutral-800 bg-neutral-900">
		<div class="mx-auto flex max-w-5xl items-center justify-between gap-4 px-6 py-3">
			<a href="/admin" class="text-lg font-semibold tracking-tight hover:text-white">
				monokasa <span class="text-neutral-500">· admin</span>
			</a>
			<div class="flex items-center gap-3 text-sm">
				{#if me}
					<span class="text-neutral-400">{me.email}</span>
					<button
						onclick={logout}
						class="rounded-md border border-neutral-700 px-3 py-1 text-neutral-300 hover:bg-neutral-800"
					>
						Вийти
					</button>
				{/if}
			</div>
		</div>
	</header>

	{#if !loaded}
		<div class="mx-auto max-w-5xl px-6 py-12 text-center text-neutral-500">
			Перевіряю сесію…
		</div>
	{:else}
		<main class="mx-auto max-w-5xl px-6 py-6">
			{@render children()}
		</main>
	{/if}
</div>
