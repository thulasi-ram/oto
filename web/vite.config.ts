import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// The UI consumes only the public HTTP API (SPEC §0.2). In development the Vite
// server proxies every backend path straight through to the Go process on :8080,
// so the browser never learns there are two origins and there is no CORS in the
// happy path.
const backend = process.env.OTO_BACKEND_URL ?? "http://localhost:8080";

export default defineConfig({
  plugins: [solid(), tailwindcss()],
  resolve: {
    alias: { "~": path.resolve(import.meta.dirname, "src") },
  },
  server: {
    port: 5173,
    proxy: {
      "/api": { target: backend, changeOrigin: true },
      "/healthz": { target: backend, changeOrigin: true },
      "/readyz": { target: backend, changeOrigin: true },
      "/metrics": { target: backend, changeOrigin: true },
    },
  },
  build: {
    outDir: "dist",
    sourcemap: true,
    target: "es2022",
  },
  test: {
    environment: "jsdom",
    globals: true,
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
