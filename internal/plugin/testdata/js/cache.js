export function prepare_request(ctx) {
  const key = ctx.input.text || "k";
  if (ctx.options.action === "delete") {
    return {
      body: {kind: "json", value: {deleted: lax.cache.delete("shell", key), after: lax.cache.get("shell", key)}},
    };
  }
  const before = lax.cache.get("shell", key);
  const ttl = Number(ctx.options.ttl_ms || 0);
  lax.cache.set("shell", key, {n: (before ? before.n : 0) + 1, now: lax.now()}, ttl);
  return {body: {kind: "json", value: {before: before, after: lax.cache.get("shell", key)}}};
}
