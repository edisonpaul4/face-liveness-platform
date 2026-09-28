import { sveltekit } from '@sveltejs/vite-plugin-svelte';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		port: 5173,
		proxy: {
			// El gateway (Go) termina el WebSocket.
			'/ws': { target: 'ws://localhost:8080', ws: true },
			'/api': { target: 'http://localhost:8081', changeOrigin: true }
		}
	}
});
