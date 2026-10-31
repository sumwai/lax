// 两个文件互相 import：a 先加载，b 加载时拿到的 a 是半成品 exports。
import {fromB} from "./cycle-b.js";

export function fromA(value) {
  return "a(" + value + ")";
}

export function prepare_request(ctx) {
  return {body: {kind: "json", value: {a: fromB(ctx.input.text)}}};
}
