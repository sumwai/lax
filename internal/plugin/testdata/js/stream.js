// 模块级变量只在一条流里延续：同一个 Runtime 连着解块，新流换 Runtime。
let moduleCounter = 0;

export function decode_stream(ctx) {
  moduleCounter += 1;
  const incoming = ctx.state ? ctx.state.chunks : null;
  const state = ctx.state || {chunks: 0};
  state.chunks += 1;
  const events = [];
  if (ctx.chunk.data) {
    events.push({
      type: "text.delta",
      text: "data=" + ctx.chunk.data + " state=" + (incoming === null ? "null" : incoming) + " module=" + moduleCounter,
    });
  }
  if (ctx.chunk.final) {
    events.push({type: "finish", reason: "stop"});
  }
  return {events: events, state: state, done: false};
}
