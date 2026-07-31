import { readdir, stat } from "node:fs/promises";
import { gzipSync } from "node:zlib";
import { readFile } from "node:fs/promises";
import { resolve } from "node:path";

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

await walk(root);
const initial = files.filter((path) => /\.(js|css)$/.test(path) && !path.includes("Chart-"));
let compressed = 0;
for (const path of initial) compressed += gzipSync(await readFile(path)).byteLength;
const limit = 500 * 1024;
if (compressed > limit) {
  throw new Error(`Initial bundle ${compressed} bytes gzip exceeds ${limit} byte budget`);
}
console.log(`Initial bundle: ${compressed} bytes gzip (${initial.length} files)`);
