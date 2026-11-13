import tailwindcss from '@tailwindcss/vite';
import react from '@vitejs/plugin-react';
import path from 'path';
import {defineConfig} from 'vite';

// Dev-only: point the dev server at a real anchor so /api/mesh has data.
// Set CMESH_ANCHOR (e.g. http://127.0.0.1:18090) before `npm run dev`.
const anchor = process.env.CMESH_ANCHOR;

export default defineConfig(() => {
  return {
    plugins: [react(), tailwindcss()],
    resolve: {
      alias: {
        '@': path.resolve(import.meta.dirname ?? '.'),
      },
    },
    // The product serves the built app from the Go binary, embedded at
    // /panel. Relative asset paths keep it working from any route the
    // anchor mounts it under.
    base: './',
    build: {
      outDir: 'dist',
      emptyOutDir: true,
      // The app is served from disk inside a single binary; keep the
      // sourcemaps out of the release.
      sourcemap: false,
      chunkSizeWarningLimit: 1500,
    },
    server: anchor
      ? {
          proxy: {
            '/api/mesh': {target: anchor, changeOrigin: true},
          },
        }
      : undefined,
  };
});
