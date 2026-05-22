<script lang="ts">
	import { api, type AuditEntry, ApiError } from '$lib/api';

	let entries = $state<AuditEntry[]>([]);
	let loaded = $state(false);
	let error = $state('');

	$effect(() => {
		(async () => {
			try {
				entries = await api.get<AuditEntry[]>('/api/admin/audit');
			} catch (e) {
				if (e instanceof ApiError) error = e.detail || e.code;
				else error = String(e);
			} finally {
				loaded = true;
			}
		})();
	});

	function formatDateTime(iso: string): string {
		return new Date(iso).toLocaleString('uk-UA', {
			day: '2-digit',
			month: '2-digit',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit'
		});
	}

	// Human label per action. Unknowns fall back to the raw key so a new
	// audit kind doesn't blank the column.
	const actionLabels: Record<string, string> = {
		'show.create': 'створено подію',
		'show.update': 'оновлено подію',
		'show.archive': 'архівовано подію',
		'seat.add': 'додано місце',
		'seat.remove': 'видалено місце',
		'seat.batch_update': 'оновлено місця',
		'reservation.cancel': 'скасовано бронь',
		'order.create': 'бронювання',
		'payment.confirm': 'оплата',
		'order.refund_marked': 'позначено повернутим'
	};

	// Tint each action so admin/buyer/system entries are easy to skim.
	function actionTint(a: string): string {
		if (a === 'payment.confirm') return 'text-emerald-300';
		if (a === 'order.create') return 'text-amber-300';
		if (a === 'reservation.cancel') return 'text-red-300';
		if (a === 'order.refund_marked') return 'text-violet-300';
		return 'text-neutral-200';
	}

	// Actor label varies by source: admin email, buyer email, @tg-username,
	// or empty (for system events with no natural identity).
	function actorLabel(e: AuditEntry): string {
		if (e.actor_user_id > 0) return e.actor_email; // admin
		if (e.actor_email) return e.actor_email; // buyer / bot
		return 'система';
	}

	// Format the details JSON compactly — single-line key=value pairs.
	function formatDetails(d: unknown): string {
		if (d == null || typeof d !== 'object') return '';
		return Object.entries(d as Record<string, unknown>)
			.map(([k, v]) => {
				if (Array.isArray(v)) return `${k}=[${v.length}]`;
				if (typeof v === 'object') return `${k}={…}`;
				return `${k}=${String(v)}`;
			})
			.join(' · ');
	}
</script>

<svelte:head>
	<title>monokasa · журнал дій</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← До подій</a>
</div>

<h1 class="mt-2 text-2xl font-semibold tracking-tight">Журнал</h1>
<p class="mt-1 text-sm text-neutral-400">
	Усе що відбувається: бронювання, оплати, скасування, edits від адмінів.
	Зелене — оплата, жовте — нова бронь, червоне — скасування.
</p>

{#if error}
	<div class="mt-4 rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
		{error}
	</div>
{:else if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else if entries.length === 0}
	<div class="mt-6 rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center text-neutral-400">
		Ще нічого не сталося.
	</div>
{:else}
	<div class="mt-4 overflow-x-auto rounded-lg border border-neutral-800">
		<table class="w-full text-sm">
			<thead class="bg-neutral-900 text-left text-xs uppercase tracking-wider text-neutral-500">
				<tr>
					<th class="px-3 py-2">Час</th>
					<th class="px-3 py-2">Адмін</th>
					<th class="px-3 py-2">Дія</th>
					<th class="px-3 py-2">Об'єкт</th>
					<th class="px-3 py-2">Деталі</th>
				</tr>
			</thead>
			<tbody class="divide-y divide-neutral-900">
				{#each entries as e (e.id)}
					<tr class="hover:bg-neutral-900/50">
						<td class="px-3 py-2 text-xs text-neutral-500 whitespace-nowrap">
							{formatDateTime(e.created_at)}
						</td>
						<td class="px-3 py-2 text-neutral-300">{actorLabel(e)}</td>
						<td class="px-3 py-2 {actionTint(e.action)}">{actionLabels[e.action] ?? e.action}</td>
						<td class="px-3 py-2 font-mono text-xs text-neutral-400">{e.target}</td>
						<td class="px-3 py-2 text-xs text-neutral-500">
							{formatDetails(e.details)}
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	</div>
{/if}
