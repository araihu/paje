import { cp, mkdir, rm } from "node:fs/promises";
import { resolve } from "node:path";

const root = process.cwd();
const dist = resolve(root, "dist");

// Run after Vite finishes both Worker and fallback-client builds. The Go
// generator owns every public document; Sites receives that exact asset tree.
await rm(resolve(dist, "client"), { recursive: true, force: true });
await cp(resolve(root, "public"), resolve(dist, "client"), { recursive: true });
await rm(resolve(dist, "server"), { recursive: true, force: true });
await cp(resolve(dist, "paje_site"), resolve(dist, "server"), { recursive: true });
await mkdir(resolve(dist, ".openai"), { recursive: true });
await cp(resolve(root, ".openai", "hosting.json"), resolve(dist, ".openai", "hosting.json"));
