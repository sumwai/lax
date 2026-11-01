#!/bin/sh
set -eu

archive=${1:?缺少发布包路径}
expected=${2:?缺少期望版本}
service=${3:-lax.service}
health_url=${4:-http://127.0.0.1:4567/healthz}
health_timeout=${5:-30}

bindir="$HOME/.local/bin"
binary="$bindir/lax"
previous="$HOME/deploy/lax.previous"
tmp=$(mktemp -d "$HOME/.lax-install.XXXXXX")

cleanup() {
  rm -rf "$tmp"
  rm -f "$archive"
}
trap cleanup EXIT HUP INT TERM

tar -xzf "$archive" -C "$tmp"
if [ ! -f "$tmp/lax" ]; then
  echo "发布包里没有 lax 二进制" >&2
  exit 1
fi
chmod 0755 "$tmp/lax"

actual=$("$tmp/lax" version | awk '{print $NF}')
if [ "$actual" != "$expected" ]; then
  echo "发布包版本是 $actual，期望 $expected" >&2
  exit 1
fi

mkdir -p "$bindir" "$HOME/deploy"
if [ -x "$binary" ]; then
  cp -p "$binary" "$previous"
fi

new_binary="$bindir/.lax.new.$$"
cp "$tmp/lax" "$new_binary"
chmod 0755 "$new_binary"
mv -f "$new_binary" "$binary"

restart() {
  sudo -n systemctl restart "$service"
}

rollback() {
  if [ -x "$previous" ]; then
    install -m 0755 "$previous" "$binary"
    restart || true
    echo "已回滚到上一次的二进制：$previous" >&2
  else
    echo "没有可回滚的旧二进制" >&2
  fi
}

if ! restart; then
  echo "重启 $service 失败" >&2
  rollback
  exit 1
fi

i=0
while [ "$i" -lt "$health_timeout" ]; do
  if body=$(curl -fsS --max-time 2 "$health_url" 2>/dev/null); then
    case "$body" in
      *'"status":"ok"'*)
        echo "健康检查通过：$health_url"
        exit 0
        ;;
    esac
  fi
  i=$((i + 1))
  sleep 1
done

echo "健康检查连续 $health_timeout 秒未通过：$health_url" >&2
rollback
exit 1
