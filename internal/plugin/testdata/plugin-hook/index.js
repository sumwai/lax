// 最小 hook 插件：请求体固定几个字段，响应固定回一小段 mp3 字节。
// 用例只验接线，不验上游行为，所以两个钩子都不发网络请求。
//
// 用 export default 一个对象：钩子清单一眼能看出来，沙箱按 module.exports 的键取钩子。
// 钩子按文档写成 async：真实插件都这么写，用例顺带验沙箱的 Promise 解析。

export default {
  async prepare_request(ctx) {
    return {
      path: "/v1/audio/speech",
      body: {
        kind: "json",
        value: {
          model: ctx.model,
          input: ctx.input.text,
          voice: ctx.input.voice || "alloy"
        }
      }
    };
  },

  async decode_response(ctx) {
    return {
      result: {kind: "media", mime: "audio/mpeg", body_base64: "SUQz"},
      usage: {tts_characters: ctx.input.text.length}
    };
  }
};
