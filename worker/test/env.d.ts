import type { Env as WorkerEnv } from "../src/index";

// Types `env` from cloudflare:workers with the bindings in wrangler.example.toml.
declare global {
  namespace Cloudflare {
    interface Env extends WorkerEnv {}
  }
}
