import { fileURLToPath, URL } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// La build finisce in internal/webui/dist, incorporata in manent-server con go:embed.
// In sviluppo (npm run dev) le chiamate all'API vanno a un manent locale (MANENT_DEV_API).
const api = process.env.MANENT_DEV_API ?? "http://127.0.0.1:8080";

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    sourcemap: false,
    chunkSizeWarningLimit: 700,
    rollupOptions: {
      output: {
        manualChunks: { react: ["react", "react-dom", "react-router-dom"] },
      },
    },
  },
  server: {
    proxy: { "/api": api, "/ui/config.json": api, "/healthz": api },
  },
  test: {
    environment: "jsdom",
    globals: true,
    setupFiles: ["./src/test/setup.ts"],
    include: ["src/**/*.test.{ts,tsx}"],
    css: false,
  },
});
