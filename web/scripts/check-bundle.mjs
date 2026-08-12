import { readFile, readdir, stat } from "node:fs/promises";
import { gzipSync } from "node:zlib";
import { basename, resolve } from "node:path";

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

// The initial graph is whatever index.html tells the browser to fetch before it
// can render: the entry script, its modulepreloads, and the stylesheet. Chunks
// reached through a dynamic import() are absent from it by construction, which
// is what makes this a measurement rather than a filename convention — the
// previous rule excluded anything named "Chart-", so a newly split chunk was
// silently counted as initial and a chunk that stopped being split was silently
// not.
const html = await readFile(resolve(root, "index.html"), "utf8");
const referenced = new Set([...html.matchAll(/(?:src|href)="[^"]*\/assets\/([^"]+)"/g)].map((match) => match[1]));
if (referenced.size === 0) throw new Error("index.html references no assets — the entry graph could not be read");

const assetFor = (name) => {
  const path = files.find((candidate) => basename(candidate) === name);
  if (!path) throw new Error(`index.html references ${name}, which is not in the build output`);
  return path;
};

// Startup blocks on exactly one locale, so the worst case belongs in the budget
// even though the locale chunk is loaded dynamically. The other one is only
// fetched if the operator switches language.
const locales = ["zh-CN", "en-US"].map((locale) => {
  const path = files.find((candidate) => new RegExp(`^${locale}-[^/]+\\.js$`).test(basename(candidate)));
  if (!path) {
    throw new Error(
      `Locale ${locale} has no chunk of its own — something imports it statically, ` +
        `which puts every locale in every session's initial download`,
    );
  }
  if (referenced.has(basename(path))) {
    throw new Error(`Locale ${locale} is preloaded from index.html; it should be reached by dynamic import only`);
  }
  return path;
});

const gzipOf = async (path) => gzipSync(await readFile(path)).byteLength;

let compressed = 0;
for (const name of referenced) compressed += await gzipOf(assetFor(name));
const localeSizes = await Promise.all(locales.map(gzipOf));
compressed += Math.max(...localeSizes);

const limit = 500 * 1024;
if (compressed > limit) {
  throw new Error(`Initial bundle ${compressed} bytes gzip exceeds ${limit} byte budget`);
}
console.log(`Initial bundle: ${compressed} bytes gzip (${referenced.size} entry assets + heaviest locale)`);
