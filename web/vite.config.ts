import { fileURLToPath, URL } from "node:url";
import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// The build ends up in internal/webui/dist, embedded in manent-server with go:embed.
// In development (npm run dev) API calls go to a local manent (MANENT_DEV_API).
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
