import { readFile, readdir, stat } from "node:fs/promises";
import { relative, resolve } from "node:path";

const root = resolve(import.meta.dirname, "../../internal/webui/dist");
const files = [];

async function walk(directory) {
  for (const entry of await readdir(directory)) {
    const path = resolve(directory, entry);
    const info = await stat(path);
    if (info.isDirectory()) await walk(path);
    else files.push(path);
  }
}

const forbidden = [
  "sk-HEIMDALL_",
  "AIzaHEIMDALL",
  "ASIA0123456789ABCDEF",
  "heimdall.canary.token",
  "provider-secret-canary",
  "gw_plaintext-canary",
  "csrf-canary",
  "password-canary",
  "correct horse battery staple",
  "sourceMappingURL=",
  "localStorage",
  "sessionStorage",
  "indexedDB",
];

await walk(root);
for (const path of files) {
  const name = relative(root, path);
  if (name.endsWith(".map")) throw new Error(`Source map in production artifact: ${name}`);
  const payload = await readFile(path, "utf8");
  for (const canary of forbidden) {
    if (payload.includes(canary)) throw new Error(`Forbidden secret/storage marker ${canary} in ${name}`);
  }
}
console.log(`Browser artifact secret scan: ${files.length} files clean`);
