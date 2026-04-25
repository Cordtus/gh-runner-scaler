import { createHash } from "node:crypto";
import { mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import path from "node:path";

import { build } from "esbuild";

const shard = Number.parseInt(process.argv[2] ?? "0", 10);
const fileCount = Number.parseInt(process.argv[3] ?? "180", 10);
const sizeKB = Number.parseInt(process.argv[4] ?? "24", 10);
const bundleRepeats = Number.parseInt(process.argv[5] ?? "3", 10);

const outDir = path.join("tmp", `node-shard-${shard}`);
rmSync(outDir, { recursive: true, force: true });
mkdirSync(outDir, { recursive: true });

const chunk = "runner-load-lab-".repeat(Math.max(1, Math.ceil((sizeKB * 1024) / 16)));
for (let index = 0; index < fileCount; index += 1) {
  const payload = JSON.stringify({
    shard,
    index,
    content: chunk.slice(0, sizeKB * 1024)
  });
  writeFileSync(path.join(outDir, `fixture-${index}.json`), payload);
}

const digest = createHash("sha256");
for (const entry of readdirSync(outDir).sort()) {
  digest.update(readFileSync(path.join(outDir, entry)));
}

for (let index = 0; index < bundleRepeats; index += 1) {
  await build({
    entryPoints: ["src/index.ts"],
    bundle: true,
    platform: "node",
    format: "esm",
    outfile: path.join(outDir, `bundle-${index}.js`)
  });
}

writeFileSync(
  path.join(outDir, "summary.json"),
  JSON.stringify({
    shard,
    fileCount,
    sizeKB,
    bundleRepeats,
    digest: digest.digest("hex")
  }, null, 2)
);

console.log(`node workload complete for shard ${shard}`);
