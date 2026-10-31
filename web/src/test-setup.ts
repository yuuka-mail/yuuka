import { overwriteGetLocale } from "@/paraglide/runtime.js";

// The source language: tests assert its wording, and Node's
// navigator.language would otherwise pick by the machine's locale.
overwriteGetLocale(() => "en");
