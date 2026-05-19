import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		// adapter-static gives a fully prerendered + CSR-fallback build that
		// the Go backend can serve through embed.FS in production. The
		// fallback (index.html for any unknown path) is what lets the SPA
		// client-side routing handle /admin/* and /events/* without 404s.
		adapter: adapter({
			pages: 'build',
			assets: 'build',
			fallback: 'index.html',
			precompress: false,
			strict: false
		})
	}
};

export default config;
