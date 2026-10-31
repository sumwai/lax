import fs from "node:fs";

export function prepare_request() {
  return {body: {kind: "json", value: {}}};
}
