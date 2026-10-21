import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import { viteSingleFile } from 'vite-plugin-singlefile';
import path from 'node:path';

export default defineConfig({
  plugins: [react(), tailwindcss(), viteSingleFile()],
  resolve: {
    alias: { '@': path.resolve(__dirname, 'src') },
  },
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:3000',
    },
  },
  build: {
    target: 'es2020',
    cssMinify: true,
    assetsInlineLimit: 100_000_000,
    chunkSizeWarningLimit: 3_000,
  },
});
