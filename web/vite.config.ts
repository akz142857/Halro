import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  base: "/admin/",
  plugins: [react()],
  build: {
    outDir: "../internal/webui/dist",
    emptyOutDir: true,
    sourcemap: false,
    assetsInlineLimit: 0,
    rollupOptions: {
      output: {
        manualChunks(id) {
          if (id.includes("/node_modules/react/") || id.includes("/node_modules/react-dom/")) {
            return "react";
          }
          if (id.includes("/node_modules/@tanstack/react-query/") || id.includes("/node_modules/@tanstack/react-table/")) {
            return "query";
          }
        },
      },
    },
  },
  server: {
    port: 5173,
    strictPort: true,
    proxy: {
      "/admin/api": "http://127.0.0.1:8081",
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: "./src/test/setup.ts",
    restoreMocks: true,
    // Pinned so the host's own zone can never stand in for the one the code is
    // told to use. On a machine already set to the zone a fixture names, a
    // regression that ignored the server's time zone would still pass.
    env: { TZ: "America/New_York" },
  },
});
