// JSON.parse 造出来的 __proto__ 是自有属性，宿主必须拒绝。
export function prepare_request() {
  const value = JSON.parse('{"__proto__":"injected","ok":true}');
  return {body: {kind: "json", value: value}};
}
