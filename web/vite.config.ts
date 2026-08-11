import { defineConfig } from "vite";
import solid from "vite-plugin-solid";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// The UI consumes only the public HTTP API (SPEC §0.2). In development the Vite
// server proxies every backend path straight through to the Go process on :8080,
// so the browser never learns there are two origins and there is no CORS in the
// happy path.
const backend = process.env.OTO_BACKEND_URL ?? "http://localhost:8080";

/**
 * Under vitest, Solid must resolve through its `development` export condition.
 *
 * Without it `@solidjs/testing-library` and the components under test each load
 * a different build of `solid-js`, the runtime prints "you appear to have
 * multiple instances of Solid", and context — the query client, the router —
 * silently fails to cross the boundary. It is scoped to the test run on purpose:
 * shipping the development condition in `vite build` would ship the dev runtime.
 */
export default defineConfig(({ mode }) => ({
  plugins: [solid(), tailwindcss()],
  resolve: {
    alias: { "~": path.resolve(import.meta.dirname, "src") },
    ...(mode === "test" ? { conditions: ["development", "browser"] } : {}),
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
    setupFiles: ["src/test/setup.ts"],
    // Solid must be loaded through vite-plugin-solid's *test* transform rather
    // than resolved to its browser build, or every component under test gets a
    // second copy of the reactive runtime and nothing re-renders.
    //
    // ⛔ `@tanstack/solid-query` belongs in this list too. Left out, it resolves
    // its own copy of `solid-js`, the runtime warns "multiple instances of
    // Solid", and — the part that matters — a query that resolves after mount
    // updates a store the component's tracking scope never subscribed to. The
    // screen renders its loading shape forever and the test that waits for data
    // times out with no other symptom.
    server: { deps: { inline: [/solid-js/, /@solidjs/, /@tanstack\/solid-query/] } },
  },
}));
