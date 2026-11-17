// Guard against a class of bug that only shows up at runtime:
//
//   invoke() looks the method up in desktopMethods by name. If a new backend
//   call is added to clientApi.js but not registered in desktopMethods, the
//   call is rejected inside the UI, so nothing reaches Go and the log stays
//   empty. The build still succeeds, which makes this easy to miss.
//
// Run: node scripts/check-desktop-methods.mjs [src/services/clientApi.js]
import { readFileSync } from "node:fs";

const file = process.argv[2] || "src/services/clientApi.js";
const text = readFileSync(file, "utf8");

const mapStart = text.indexOf("const desktopMethods = {");
if (mapStart < 0) {
  console.error("could not locate `const desktopMethods = {`");
  process.exit(2);
}
// The literal ends at the first line that is exactly "};".
const mapEnd = text.indexOf("\n};", mapStart);
if (mapEnd < 0) {
  console.error("could not locate the end of desktopMethods");
  process.exit(2);
}
const map = text.slice(mapStart, mapEnd);

// Every call site that dispatches by method name.
const callPatterns = [
  /invoke\(\s*"[^"]*"\s*,\s*"([A-Za-z0-9_]+)"/g,
  /desktopOrMockRaw\(\s*[^,]+,\s*"[^"]*"\s*,\s*"([A-Za-z0-9_]+)"/g,
  /desktopOrMock\(\s*[^,]+,\s*"[^"]*"\s*,\s*"([A-Za-z0-9_]+)"/g,
];

const invoked = new Set();
for (const pattern of callPatterns) {
  for (const match of text.matchAll(pattern)) {
    invoked.add(match[1]);
  }
}

const missing = [...invoked]
  .filter((name) => !new RegExp(`(^|[^A-Za-z0-9_])${name}([^A-Za-z0-9_]|$)`, "m").test(map))
  .sort();

console.log(`checked ${invoked.size} dispatched method name(s) against desktopMethods`);
if (missing.length === 0) {
  console.log("all dispatched methods are registered");
  process.exit(0);
}
console.error(`unregistered methods (${missing.length}):`);
for (const name of missing) console.error(`  - ${name}`);
console.error("\nEach one is rejected inside the UI before reaching the backend.");
process.exit(1);