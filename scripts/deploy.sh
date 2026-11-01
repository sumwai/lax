#!/bin/sh
set -eu

usage() {
  echo "用法：deploy.sh <发布包> <版本>" >&2
  echo "环境变量：DEPLOY_HOST、DEPLOY_USER、DEPLOY_PORT、DEPLOY_SSH_KEY、DEPLOY_KNOWN_HOSTS、DEPLOY_HEALTH_TIMEOUT" >&2
}

archive=${1:-}
version=${2:-}
if [ -z "$archive" ] || [ -z "$version" ]; then
  usage
  exit 2
fi

version=${version#v}
case "$version" in
  ""|*[!0-9A-Za-z.+-]*)
    echo "版本号只能包含数字、字母、点、加号与连字符：$version" >&2
    exit 2
    ;;
esac

if [ ! -f "$archive" ]; then
  echo "发布包不存在：$archive" >&2
  exit 2
fi

: "${DEPLOY_HOST:?请设置 DEPLOY_HOST（部署目标主机）}"
: "${DEPLOY_SSH_KEY:?请设置 DEPLOY_SSH_KEY（部署私钥路径）}"
: "${DEPLOY_KNOWN_HOSTS:?请设置 DEPLOY_KNOWN_HOSTS（固定主机指纹文件）}"

DEPLOY_USER=${DEPLOY_USER:-sumwai}
DEPLOY_PORT=${DEPLOY_PORT:-22}
DEPLOY_SERVICE=${DEPLOY_SERVICE:-lax.service}
DEPLOY_HEALTH_URL=${DEPLOY_HEALTH_URL:-http://127.0.0.1:4567/healthz}
DEPLOY_HEALTH_TIMEOUT=${DEPLOY_HEALTH_TIMEOUT:-30}

case "$DEPLOY_HEALTH_TIMEOUT" in
  ""|0|*[!0-9]*)
    echo "DEPLOY_HEALTH_TIMEOUT 必须是正整数秒：$DEPLOY_HEALTH_TIMEOUT" >&2
    exit 2
    ;;
esac

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
remote_script="$script_dir/deploy-remote.sh"
if [ ! -f "$remote_script" ]; then
  echo "远端部署脚本不存在：$remote_script" >&2
  exit 2
fi

target="$DEPLOY_USER@$DEPLOY_HOST"

ssh_cmd() {
  ssh -F /dev/null \
    -o BatchMode=yes \
    -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=yes \
    -o "UserKnownHostsFile=$DEPLOY_KNOWN_HOSTS" \
    -o ConnectTimeout=15 \
    -i "$DEPLOY_SSH_KEY" \
    "$@"
}

scp_cmd() {
  scp -F /dev/null \
    -o BatchMode=yes \
    -o IdentitiesOnly=yes \
    -o StrictHostKeyChecking=yes \
    -o "UserKnownHostsFile=$DEPLOY_KNOWN_HOSTS" \
    -o ConnectTimeout=15 \
    -i "$DEPLOY_SSH_KEY" \
    "$@"
}

remote_archive=$(ssh_cmd -p "$DEPLOY_PORT" "$target" 'mktemp "$HOME/.lax-deploy.XXXXXX.tar.gz"')
cleanup() {
  ssh_cmd -p "$DEPLOY_PORT" "$target" "rm -f '$remote_archive'" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM

echo "上传 $version 发布包到 $target:$remote_archive"
scp_cmd -P "$DEPLOY_PORT" "$archive" "$target:$remote_archive"

echo "在 $target 上安装并重启 $DEPLOY_SERVICE"
ssh_cmd -p "$DEPLOY_PORT" "$target" \
  "sh -s -- '$remote_archive' '$version' '$DEPLOY_SERVICE' '$DEPLOY_HEALTH_URL' '$DEPLOY_HEALTH_TIMEOUT'" \
  < "$remote_script"

echo "部署完成：$target 上的 lax 已是 $version"
