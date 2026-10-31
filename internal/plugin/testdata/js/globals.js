// 沙箱里不该出现的全局一律 undefined。
export function prepare_request() {
  const names = ["fetch", "process", "require", "Bun", "child_process", "XMLHttpRequest", "WebSocket", "fs", "setTimeout", "eval", "Function"];
  const value = {};
  for (const name of names) {
    value[name] = typeof globalThis[name];
  }
  value.hasFetch = "fetch" in globalThis;
  value.url = typeof URL;
  value.lax = typeof lax;
  value.console = typeof console;
  return {body: {kind: "json", value: value}};
}
