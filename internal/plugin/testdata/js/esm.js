// 多行具名 import、as 别名、export const 与 export default 对象。
import {
  greet,
  suffix as tail,
} from "./esm-helpers.js";

export const marker = "esm";

export default {
  prepare_request(ctx) {
    return {
      body: {
        kind: "json",
        value: {greeting: greet(ctx.input.text) + tail, uuid: typeof lax.randomUUID(), marker: marker},
      },
    };
  },
};
