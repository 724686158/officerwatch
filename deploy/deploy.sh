#!/usr/bin/env bash
# 把 officerwatch 发布到一台 Linux 主机（ssh + systemd + 可选 Nginx 片段）。
#
#   DEPLOY_HOST=example.com DEPLOY_USER=me ./deploy/deploy.sh
#
# 可覆盖的变量：
#   APP_DIR      远端程序目录（默认 /opt/officerwatch）
#   APP_USER     运行用户（默认 = DEPLOY_USER）
#   PORT         监听端口，写进 Nginx 片段与自测（默认 8795，需与 config.json 的 listen 一致）
#   SSH_ARGS     额外的 ssh/scp 参数（如 -i ~/.ssh/key -p 2222）
#   SKIP_NGINX=1 只装二进制和 unit，不碰 Nginx
#
# 幂等：重复跑不会动远端的 data/ 与 config.json。首次启动时程序会自己生成一份
# config.json 模板（0600），填好 SMTP 再重启即可开始发信。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
APP=officerwatch

: "${DEPLOY_HOST:?请设置 DEPLOY_HOST（例如 DEPLOY_HOST=example.com）}"
DEPLOY_USER="${DEPLOY_USER:-$(whoami)}"
APP_DIR="${APP_DIR:-/opt/$APP}"
APP_USER="${APP_USER:-$DEPLOY_USER}"
PORT="${PORT:-8795}"
SSH_ARGS="${SSH_ARGS:--o BatchMode=yes -o ConnectTimeout=15}"
VERSION="$(date +%Y%m%d-%H%M%S)"
TARGET="$DEPLOY_USER@$DEPLOY_HOST"

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
remote() { ssh $SSH_ARGS "$TARGET" "$@"; }

say "交叉编译 linux/amd64（version=$VERSION）"
cd "$ROOT"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=-mod=mod \
  go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /tmp/$APP ./cmd/officerwatch
ls -lh /tmp/$APP | awk '{print "    二进制：" $5}'

say "打包并渲染模板"
STAGE="$(mktemp -d)"
mkdir -p "$STAGE/deploy"
cp deploy/officerwatch.service deploy/nginx-officerwatch.conf "$STAGE/deploy/"
cp /tmp/$APP "$STAGE/$APP"
sed -i.bak -e "s|__APP_DIR__|$APP_DIR|g" -e "s|__APP_USER__|$APP_USER|g" "$STAGE/deploy/officerwatch.service"
sed -i.bak -e "s|__PORT__|$PORT|g" "$STAGE/deploy/nginx-officerwatch.conf"
rm -f "$STAGE"/deploy/*.bak

TARBALL="$(mktemp -t $APP-deploy).tgz"
LC_ALL=C tar --no-xattrs -czf "$TARBALL" -C "$STAGE" .
rm -rf "$STAGE"

say "上传到 $TARGET"
scp $SSH_ARGS "$TARBALL" "$TARGET:/tmp/$APP-deploy.tgz"

say "远端安装"
remote "set -e
  sudo mkdir -p '$APP_DIR/bin' '$APP_DIR/data'
  sudo chown -R '$APP_USER:$APP_USER' '$APP_DIR'
  rm -rf /tmp/$APP-unpack && mkdir -p /tmp/$APP-unpack
  LC_ALL=C tar -xzf /tmp/$APP-deploy.tgz -C /tmp/$APP-unpack
  install -m 755 /tmp/$APP-unpack/$APP '$APP_DIR/bin/$APP'
  sudo install -m 644 /tmp/$APP-unpack/deploy/$APP.service /etc/systemd/system/$APP.service
  if [ '${SKIP_NGINX:-0}' != '1' ] && [ -d /etc/nginx/snippets ]; then
    sudo install -m 644 /tmp/$APP-unpack/deploy/nginx-$APP.conf /etc/nginx/snippets/app-$APP.conf
    sudo nginx -t && sudo systemctl reload nginx
  fi
  sudo systemctl daemon-reload
  sudo systemctl enable $APP >/dev/null 2>&1 || true
  sudo systemctl restart $APP
  rm -rf /tmp/$APP-unpack /tmp/$APP-deploy.tgz
  sleep 2
  systemctl is-active $APP
  curl -s -o /dev/null -w '    本机自测 /healthz: %{http_code}\n' http://127.0.0.1:$PORT/healthz"

rm -f "$TARBALL" /tmp/$APP
say "完成，版本 $VERSION"
