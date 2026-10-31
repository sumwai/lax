// 对话操作的 hook 方言：请求把宿主编好的报文原样带回并补一个字段，
// 响应把正文改写过一次。用例只验接线，不发网络请求。

export default {
  async prepare_request(ctx) {
    const body = {};
    for (const key in ctx.body) {
      body[key] = ctx.body[key];
    }
    body.enable_thinking = true;
    return {body: body};
  },

  async decode_response(ctx) {
    return {result: {kind: "json", json: ctx.response.body}};
  }
};
