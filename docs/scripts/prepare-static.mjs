import { cp, mkdir, readdir, writeFile } from "node:fs/promises";

const client = new URL("../build/client/", import.meta.url);
const docs = new URL("docs/", client);

await mkdir(docs, { recursive: true });
await cp(new URL("assets/", client), new URL("assets/", docs), { recursive: true });

for (const entry of await readdir(client, { withFileTypes: true })) {
  if (entry.isFile() && /\.(svg|png|ico)$/.test(entry.name)) {
    await cp(new URL(entry.name, client), new URL(entry.name, docs));
  }
}

const legacy = new URL("glowby-oss/", docs);
await mkdir(legacy, { recursive: true });
await writeFile(new URL("index.html", legacy), `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Glowbom OSS</title>
<meta http-equiv="refresh" content="0;url=../glowbom-oss/">
<link rel="canonical" href="https://glowbom.com/docs/glowbom-oss/"></head>
<body><a href="../glowbom-oss/">Continue to Glowbom OSS</a></body></html>\n`);
