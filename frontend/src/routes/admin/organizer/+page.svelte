<script lang="ts">
	import { api, type Organizer, ApiError } from '$lib/api';
	import { safeImageSrc } from '$lib/safe';

	let loaded = $state(false);
	let saving = $state(false);
	let savedAt = $state<Date | null>(null);
	let error = $state('');

	let name = $state('');
	let bio = $state('');
	let contactEmail = $state('');
	let phone = $state('');
	let websiteURL = $state('');
	let telegramURL = $state('');
	let instagramURL = $state('');
	let facebookURL = $state('');
	let logoURL = $state('');

	$effect(() => {
		(async () => {
			try {
				const o = await api.get<Organizer>('/api/admin/organizer');
				name = o.name;
				bio = o.bio;
				contactEmail = o.contact_email;
				phone = o.phone;
				websiteURL = o.website_url;
				telegramURL = o.telegram_url;
				instagramURL = o.instagram_url;
				facebookURL = o.facebook_url;
				logoURL = o.logo_url;
			} catch (e) {
				if (e instanceof ApiError) error = e.detail || e.code;
				else error = String(e);
			} finally {
				loaded = true;
			}
		})();
	});

	async function save(e: Event) {
		e.preventDefault();
		saving = true;
		error = '';
		try {
			await api.put<Organizer>('/api/admin/organizer', {
				name,
				bio,
				contact_email: contactEmail,
				phone,
				website_url: websiteURL,
				telegram_url: telegramURL,
				instagram_url: instagramURL,
				facebook_url: facebookURL,
				logo_url: logoURL
			});
			savedAt = new Date();
		} catch (e) {
			if (e instanceof ApiError) error = e.detail || e.code;
			else error = String(e);
		} finally {
			saving = false;
		}
	}
</script>

<svelte:head>
	<title>monokasa · профіль організатора</title>
</svelte:head>

<div class="flex items-center gap-3">
	<a href="/admin" class="text-sm text-neutral-400 hover:text-neutral-200">← Назад</a>
</div>
<h1 class="mt-2 text-2xl font-semibold tracking-tight">Профіль організатора</h1>
<p class="mt-1 text-sm text-neutral-400">
	Те, що покупець бачить на сторінці <a href="/about" class="underline hover:text-neutral-200">/about</a>
	і в футері карток подій. Усі поля опційні.
</p>

{#if !loaded}
	<div class="mt-6 text-center text-neutral-500">Завантажую…</div>
{:else}
	<form onsubmit={save} class="mt-6 max-w-2xl space-y-5">
		<div>
			<label for="name" class="block text-sm text-neutral-400">Назва організатора</label>
			<input
				id="name"
				type="text"
				bind:value={name}
				maxlength="120"
				placeholder="Театр &laquo;Атлас&raquo;"
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			/>
		</div>

		<div>
			<label for="bio" class="block text-sm text-neutral-400">Про нас</label>
			<textarea
				id="bio"
				bind:value={bio}
				rows="6"
				maxlength="2000"
				placeholder="Хто ви, чим займаєтеся, чому варто прийти. Декілька рядків."
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			></textarea>
			<p class="mt-1 text-xs text-neutral-500">До 2000 символів. Рядки зберігаються.</p>
		</div>

		<div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
			<div>
				<label for="email" class="block text-sm text-neutral-400">Контактний email</label>
				<input
					id="email"
					type="email"
					bind:value={contactEmail}
					placeholder="hello@atlas.example"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
			<div>
				<label for="phone" class="block text-sm text-neutral-400">Телефон</label>
				<input
					id="phone"
					type="text"
					bind:value={phone}
					placeholder="+380 67 …"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
		</div>

		<fieldset class="space-y-3 rounded-lg border border-neutral-800 bg-neutral-900/30 p-4">
			<legend class="px-1 text-xs uppercase tracking-wider text-neutral-500">Посилання</legend>
			<div>
				<label for="web" class="block text-sm text-neutral-400">Сайт</label>
				<input
					id="web"
					type="text"
					bind:value={websiteURL}
					placeholder="https://atlas.example"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
			<div>
				<label for="tg" class="block text-sm text-neutral-400">Telegram</label>
				<input
					id="tg"
					type="text"
					bind:value={telegramURL}
					placeholder="https://t.me/atlas"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
			<div>
				<label for="ig" class="block text-sm text-neutral-400">Instagram</label>
				<input
					id="ig"
					type="text"
					bind:value={instagramURL}
					placeholder="https://instagram.com/atlas"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
			<div>
				<label for="fb" class="block text-sm text-neutral-400">Facebook</label>
				<input
					id="fb"
					type="text"
					bind:value={facebookURL}
					placeholder="https://facebook.com/atlas"
					class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-950 px-3 py-2 focus:border-neutral-600 focus:outline-none"
				/>
			</div>
		</fieldset>

		<div>
			<label for="logo" class="block text-sm text-neutral-400">Логотип (URL)</label>
			<input
				id="logo"
				type="text"
				bind:value={logoURL}
				placeholder="https://… або /posters/…"
				class="mt-1 w-full rounded-md border border-neutral-800 bg-neutral-900 px-3 py-2 focus:border-neutral-600 focus:outline-none"
			/>
			{#if logoURL}
				<img
					src={safeImageSrc(logoURL)}
					alt="Прев'ю логотипу"
					class="mt-3 max-h-32 rounded-md border border-neutral-800"
					onerror={(e: Event) => ((e.target as HTMLImageElement).style.display = 'none')}
				/>
			{/if}
		</div>

		{#if error}
			<div class="rounded-md border border-red-900 bg-red-950/50 p-3 text-sm text-red-300">
				{error}
			</div>
		{/if}

		<div class="flex items-center gap-3">
			<button
				type="submit"
				disabled={saving}
				class="rounded-md bg-[var(--color-brand)] px-4 py-2 text-sm font-medium text-black hover:bg-[var(--color-brand-hover)] disabled:opacity-50"
			>
				{saving ? 'Зберігаю…' : 'Зберегти'}
			</button>
			<a
				href="/about"
				target="_blank"
				rel="noopener"
				class="text-sm text-neutral-400 hover:text-neutral-200"
			>
				Подивитися /about ↗
			</a>
			{#if savedAt}
				<span class="text-xs text-neutral-500">
					Збережено о {savedAt.toLocaleTimeString('uk-UA')}
				</span>
			{/if}
		</div>
	</form>
{/if}
