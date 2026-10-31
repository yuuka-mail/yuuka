import { cloudflareTest } from "@cloudflare/vitest-plugin";
import { defineConfig } from "vitest/config";

export default defineConfig({
  plugins: [
    cloudflareTest({
      wrangler: { configPath: "./wrangler.example.toml" },
      miniflare: { bindings: { WEBHOOK_SECRET: "test-only-webhook-secret-0123456789abcdef" } },
    }),
  ],
});
