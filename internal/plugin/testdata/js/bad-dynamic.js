const helpers = await import("./esm-helpers.js");

export function prepare_request() {
  return {body: {kind: "json", value: {}}};
}
