// 返回值超过 MaxHookOutputBytes。放在 decode 上：decode 的超时上限比 prepare 宽，
// 造 9 MiB 返回值的时间不会把用例推到超时上。
export function decode_response() {
  return {result: {kind: "text", text: "x".repeat(9 * 1024 * 1024)}};
}
