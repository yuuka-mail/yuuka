import { rmSync } from "node:fs";
import { fileURLToPath } from "node:url";

import tailwindcss from "@tailwindcss/vite";
import react from "@vitejs/plugin-react";
import { defineConfig, type Plugin } from "vite";

// The API the dev server proxies to: `yuuka serve` with
// YUUKA_WEB_ORIGIN=http://localhost:<dev port>, so WebAuthn and the
// cross-origin check see the origin the browser uses.
const api = process.env.YUUKA_DEV_API ?? "http://127.0.0.1:31480";

// dist/ is embedded by web/embed.go, which needs the directory to exist
// in a fresh clone; it is tracked with only .gitkeep. Vite's emptyOutDir
// would delete that, so stale hashed assets are removed here instead.
function cleanAssets(): Plugin {
  return {
    name: "yuuka-clean-assets",
    apply: "build",
    buildStart() {
      rmSync(fileURLToPath(new URL("./dist/assets", import.meta.url)), { recursive: true, force: true });
    },
  };
}

export default defineConfig({
  plugins: [react(), tailwindcss(), cleanAssets()],
  resolve: {
    alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) },
  },
  server: {
    host: "localhost",
    port: 31473,
    strictPort: true,
    proxy: {
      "/api": { target: api },
      "/auth": { target: api },
    },
  },
  build: {
    emptyOutDir: false,
    // The embedded build ships no source maps; mail never needs them.
    sourcemap: false,
  },
});
