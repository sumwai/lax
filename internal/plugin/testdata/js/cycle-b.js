import {fromA} from "./cycle-a.js";

export function fromB(value) {
  return "b(" + value + ")";
}

// 只在被调用时用 a 的导出，加载期不碰它。
export function callsA(value) {
  return fromA(value);
}
