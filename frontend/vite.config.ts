import { sveltekit } from '@sveltejs/kit/vite';
import tailwindcss from '@tailwindcss/vite';
import { defineConfig } from 'vite';

// VITE_API_PROXY points at the Go backend in dev (Docker-compose service
// name "backend" → http://backend:8090; on the host without compose,
// http://localhost:8090). All /api and /admin/* requests are proxied
// there so cookies set by the backend share the Vite origin and "just
// work" against SameSite=Lax.
const apiProxy = process.env.VITE_API_PROXY ?? 'http://localhost:8090';

export default defineConfig({
	plugins: [tailwindcss(), sveltekit()],
	server: {
		host: '0.0.0.0',
		port: 5173,
		strictPort: true,
		proxy: {
			'/api': { target: apiProxy, changeOrigin: false },
			'/admin/login': { target: apiProxy, changeOrigin: false },
			'/admin/logout': { target: apiProxy, changeOrigin: false },
			'/webhook': { target: apiProxy, changeOrigin: false },
			'/scan': { target: apiProxy, changeOrigin: false }
		}
	}
});
