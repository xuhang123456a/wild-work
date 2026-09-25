#!/usr/bin/env bash
# 本地一键构建：build + vet + test + 产出 dist/wild-work.exe
#
# 为什么需要这个脚本：
#   CI 只产出带平台后缀的 dist/wild-work-<os>-<arch>，**不会**生成
#   dist/wild-work.exe；而运行目录用的是后者（AGENTS.md §6 第 0 条）。
#   不重建会导致「本地跑的二进制与源码不一致」（改了版本号却仍显示旧版本）。
#
# 用法（在仓库根目录或任意位置均可）：
#   bash build/build-local.sh            # 完整流程：build + vet + test + 编译
#   bash build/build-local.sh --fast     # 跳过测试，只 build + vet + 编译
#
# 可选环境变量：
#   DEPLOY_DIR=/d/AI/Workbuddy           # 构建成功后把 exe 复制到运行目录
#
# 退出码：0 成功；非 0 表示某一步失败（set -e 直接中止）。

set -euo pipefail

cd "$(dirname "$0")/.." # 切到仓库根目录，脚本从任何位置调用都成立

FAST=0
case "${1:-}" in
  --fast) FAST=1 ;;
  "") ;;
  *)
    echo "未知参数：$1（可用：--fast）" >&2
    exit 2
    ;;
esac

OUT=dist/wild-work.exe
VERSION=$(sed -n 's/^const Version = "\(.*\)"/\1/p' internal/app/app.go)
if [ -z "$VERSION" ]; then
  echo "[错误] 未能从 internal/app/app.go 读出 Version 常量" >&2
  exit 1
fi

echo "==> 版本 $VERSION | $(go version)"
echo "==> git $(git rev-parse --short HEAD) $(git diff --quiet || echo '(有未提交改动)')"

echo "==> go build ./..."
go build ./...

echo "==> go vet ./..."
go vet ./...

if [ "$FAST" -eq 1 ]; then
  echo "==> 跳过 go test（--fast）"
else
  echo "==> go test ./..."
  go test ./...
fi

echo "==> 编译 $OUT"
mkdir -p dist
# -H windowsgui：不弹控制台窗口（托盘程序必需）
if ! GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -ldflags "-H windowsgui" -o "$OUT" ./cmd/wild-work; then
  echo "[错误] 编译失败。若提示 Access is denied / 拒绝访问，" >&2
  echo "       说明目标 exe 正在运行，请先从托盘菜单退出 wild-work 再重试。" >&2
  exit 1
fi

echo "==> 校验产物"
ls -l "$OUT"
# 确认版本号确实进了二进制（否则说明拿到的是旧文件）
if grep -a -q "$VERSION" "$OUT"; then
  echo "    ✓ 二进制内含版本号 $VERSION"
else
  echo "    ✗ 二进制内未找到版本号 $VERSION，构建可能未生效" >&2
  exit 1
fi
go version -m "$OUT" | sed -n '2p;3p' | sed 's/^/    /'

if [ -n "${DEPLOY_DIR:-}" ]; then
  echo "==> 复制到 $DEPLOY_DIR"
  if ! cp "$OUT" "$DEPLOY_DIR/wild-work.exe"; then
    echo "[错误] 复制失败。请先退出正在运行的 wild-work（托盘图标 → 退出）再重试。" >&2
    exit 1
  fi
  echo "    ✓ 已更新 $DEPLOY_DIR/wild-work.exe"
fi

echo
echo "完成：$OUT（版本 $VERSION）"
