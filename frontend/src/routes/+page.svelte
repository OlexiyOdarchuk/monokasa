<script lang="ts">
	// Placeholder landing. Real content (admin entry, public event pages)
	// arrives in PR #4 and PR #5. The check below confirms the frontend
	// can reach the Go backend through the Vite proxy.
	let health = $state<'pending' | 'ok' | 'down'>('pending');

	async function ping() {
		try {
			const r = await fetch('/health');
			health = r.ok ? 'ok' : 'down';
		} catch {
			health = 'down';
		}
	}

	$effect(() => {
		ping();
	});
</script>

<main class="grid min-h-screen place-items-center p-6">
	<div class="max-w-md text-center">
		<h1 class="text-4xl font-semibold tracking-tight">monokasa</h1>
		<p class="mt-3 text-neutral-400">
			Friendly self-host для продажу квитків на події через monobank.
		</p>

		<div class="mt-8 inline-flex items-center gap-3 rounded-lg border border-neutral-800 bg-neutral-900 px-4 py-3 text-sm">
			<span
				class="inline-block size-2 rounded-full"
				class:bg-amber-400={health === 'pending'}
				class:bg-emerald-400={health === 'ok'}
				class:bg-red-400={health === 'down'}
			></span>
			<span class="text-neutral-300">
				{#if health === 'pending'}
					пінгую бекенд…
				{:else if health === 'ok'}
					бекенд відповідає
				{:else}
					бекенд недоступний
				{/if}
			</span>
		</div>

		<div class="mt-8 flex justify-center gap-3 text-sm">
			<a href="/admin/login" class="rounded-md bg-[var(--color-brand)] px-4 py-2 font-medium text-black hover:bg-[var(--color-brand-hover)]">
				Вхід для адміна
			</a>
		</div>
	</div>
</main>
