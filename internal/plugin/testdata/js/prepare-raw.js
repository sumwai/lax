// 第二种返回形状：body 直接是字符串。
export function prepare_request(ctx) {
  return {path: "/v1/raw", body: "raw:" + ctx.input.text};
}
