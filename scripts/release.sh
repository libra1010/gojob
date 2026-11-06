#!/usr/bin/env bash
# gojob 发布脚本：把"校验 → 交叉编译产物 → 打标签 → 建 GitHub Release"这条链固定下来。
#
# 用法：
#   scripts/release.sh v0.2.0                 # 全流程（本机没有 docker 时默认跳过镜像）
#   scripts/release.sh v0.2.0 --dry-run       # 只跑校验与构建，不打 tag、不推远端、不建 Release
#   scripts/release.sh v0.2.0 --docker        # 额外构建镜像并打 tag（需要 docker）
#   scripts/release.sh v0.2.0 --skip-checks   # 跳过校验矩阵（只在应急重传产物时用）
#
# 前置：工作区干净、Go 1.27+、（非 dry-run）有 origin 远端且 gh 已登录。
#
# 为什么值得用脚本而不是手敲：产物矩阵有 15 个文件，Go/前端/Java 三套校验里每一个都有
# "工具不在就静默跳过 = 等于没测"的坑，手敲一定会漏。版本戳在这里一次性盖进二进制
# （-X main.version=$TAG），容器里 `docker logs` 第一行就能看到跑的是哪一版。
#
# 注意 macOS 自带 bash 3.2：不要在双引号里嵌 $( ) 再套转义引号，会解析错乱。
set -euo pipefail

GO=${GO:-go}
TAG=${1:-}
if [ $# -gt 0 ]; then shift; fi
DRY=0
DOCKER=0
SKIP_CHECKS=0
for arg in "$@"; do
  case "$arg" in
    --dry-run)     DRY=1 ;;
    --docker)      DOCKER=1 ;;
    --skip-checks) SKIP_CHECKS=1 ;;
    *) printf '未知参数：%s\n' "$arg" >&2; exit 2 ;;
  esac
done

say() { printf '\n== %s\n' "$1"; }
die() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }

case "$TAG" in
  "")            printf '用法: scripts/release.sh vX.Y.Z [--dry-run] [--docker] [--skip-checks]\n' >&2; exit 2 ;;
  v[0-9]*.[0-9]*.[0-9]*) ;;
  *)             die "标签格式必须是 vX.Y.Z，实得 $TAG" ;;
esac
VER=${TAG#v}

cd "$(dirname "$0")/.." || exit 1
ROOT=$(pwd -P)
DIST=dist
REPO=$(git remote get-url origin 2>/dev/null | sed -E 's#.*github\.com[:/]##; s#\.git$##' || true)

say "0. 前置检查"
command -v "$GO" >/dev/null 2>&1 || die "找不到 go（用 GO=/path/to/go 指定）"
GOVER=$("$GO" env GOVERSION | sed 's/^go//')
case "$GOVER" in
  1.2[7-9].*|1.[3-9][0-9].*) ;;
  *) die "需要 Go 1.27 以上（本项目用到了语义化枚举与 range-over-int），当前 $GOVER" ;;
esac
GOFMT=$(command -v gofmt 2>/dev/null || echo "$("$GO" env GOROOT)/bin/gofmt")
[ -x "$GOFMT" ] || die "找不到 gofmt"
git rev-parse --git-dir >/dev/null 2>&1 || die "不是 git 仓库"
if [ -n "$(git status --porcelain)" ]; then
  # 产物必须对应一个确定的提交；dry-run 允许脏工作区（就是要在提交前试跑一次）
  if [ "$DRY" = 0 ]; then
    die "工作区不干净：请先提交或还原改动，再发布"
  fi
  printf '   WARNING 工作区不干净，dry-run 允许，但正式发布必须在干净工作区上跑\n'
fi
git rev-parse -q --verify "refs/tags/$TAG" >/dev/null && die "标签 $TAG 已存在"
if [ "$DRY" = 0 ]; then
  [ -n "$REPO" ] || die "没有 origin 远端（git remote add origin git@github.com:<you>/gojob.git）"
  command -v gh >/dev/null 2>&1 || die "需要 gh 命令行来建 Release"
  gh auth status >/dev/null 2>&1 || die "gh 未登录"
fi
printf '   仓库 %s · 标签 %s · 提交 %s · Go %s\n' "${REPO:-<无远端>}" "$TAG" \
  "$(git rev-parse --short HEAD 2>/dev/null || echo '<无提交>')" "$GOVER"

say "1. 校验"
if [ "$SKIP_CHECKS" = 0 ]; then
  UNFORMATTED=$("$GOFMT" -l cmd server worker internal scripts 2>/dev/null | sort -u || true)
  [ -z "$UNFORMATTED" ] || die "以下文件未 gofmt：$UNFORMATTED"
  "$GO" vet ./...
  "$GO" test -race -count=1 ./...
  if [ -d dashboard/node_modules ]; then
    (cd dashboard && ./node_modules/.bin/vue-tsc --noEmit -p tsconfig.json && ./node_modules/.bin/vite build >/dev/null)
    printf '   前端 vue-tsc + vite build 通过\n'
  else
    die "dashboard/node_modules 不存在：前端未经类型检查与构建就发布等于没验证（先 cd dashboard && pnpm install）"
  fi
  if command -v mvn >/dev/null 2>&1; then
    # 刻意不加 -o：Java 侧现在有单测（注册加密的黄金向量要 junit）。首次跑需要能取到依赖，
    # 只编译不跑测试等于把跨语言口径那条闸门关掉。
    (cd sdk-java && mvn -q test)
    printf '   Java SDK mvn test 通过（含注册加密跨语言黄金向量）\n'
  else
    printf '   WARNING 找不到 mvn，Java SDK 未经编译与测试校验\n'
  fi
else
  printf '   已跳过（--skip-checks）\n'
fi

say "2. 交叉编译产物 → $DIST/"
rm -rf "$DIST"
mkdir -p "$DIST"
# linux 两种架构是部署主力，darwin 给本地，windows 只可能有 server/demo——
# agent 在 Windows 上会被脚本执行器明确拒绝运行（照搬官方 AbstractScriptProcessor 的口径），
# 二进制仍然产出，便于同机混部时统一分发。
BINARIES="gojob-server gojob-demo gojob-agent"
PLATFORMS="linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64"
# 三个未压缩二进制每个平台就有 40MB+，直接传 Release 会堆到 200MB。按平台打成 tar.gz
# （Go 二进制压得动，约 3 倍），附件数也从 15 降到 5。
STAGE=$DIST/stage
mkdir -p "$STAGE"
built=0
for platform in $PLATFORMS; do
  goos=${platform%/*}
  goarch=${platform#*/}
  pkg=$goos-$goarch
  mkdir -p "$STAGE/$pkg"
  suffix=""
  if [ "$goos" = "windows" ]; then suffix=".exe"; fi
  for bin in $BINARIES; do
    CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch "$GO" build -trimpath \
      -ldflags="-s -w -X main.version=$TAG" -o "$STAGE/$pkg/$bin$suffix" ./cmd/$bin
    built=$((built + 1))
  done
  # README 打进包里，解压即有一份能读的说明
  cp README.md CHANGELOG.md "$STAGE/$pkg/"
  # 绝对路径：上一行刚 cd 进 $STAGE，相对路径的 ../dist 会解析成 dist/dist/
  (cd "$STAGE" && tar czf "$ROOT/$DIST/gojob-$TAG-$pkg.tar.gz" "$pkg")
  printf '   %-16s %s\n' "$pkg" "$(du -h "$DIST/gojob-$TAG-$pkg.tar.gz" | cut -f1)"
done
[ "$built" = 15 ] || die "产物数量 $built != 15，矩阵没跑全"
rm -rf "$STAGE"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$DIST" && sha256sum gojob-*.tar.gz > checksums.txt)
elif command -v shasum >/dev/null 2>&1; then
  (cd "$DIST" && shasum -a 256 gojob-*.tar.gz > checksums.txt)
else
  die "找不到 sha256sum / shasum，不能产出校验和"
fi

say "3. 镜像"
if [ "$DOCKER" = 1 ]; then
  command -v docker >/dev/null 2>&1 || die "--docker 但找不到 docker 命令"
  for bin in $BINARIES; do
    docker build --build-arg BUILD_TARGET="$bin" --build-arg GOJOB_VERSION="$TAG" \
      -t "gojob/$bin:$TAG" -t "gojob/$bin:latest" .
    printf '   已构建 gojob/%s:%s\n' "$bin" "$TAG"
  done
  printf '   推送：docker push gojob/<镜像> --all-tags\n'
else
  printf '   跳过（未加 --docker）。在有 docker 的机器上执行：\n'
  for bin in $BINARIES; do
    printf '     docker build --build-arg BUILD_TARGET=%s --build-arg GOJOB_VERSION=%s -t gojob/%s:%s -t gojob/%s:latest .\n' \
      "$bin" "$TAG" "$bin" "$TAG" "$bin"
  done
  # 国内机器上 docker.io 拉不动时，加 -f Dockerfile.cn（SWR 基础镜像 + goproxy.cn），其余参数同上。
  printf '   docker.io 拉不动的机器：上面每条命令加 -f Dockerfile.cn（基础镜像走华为云 SWR、模块代理走 goproxy.cn）\n'
fi

say "4. Release 说明"
if ! grep -q "^## \[$VER\]" CHANGELOG.md; then
  die "CHANGELOG.md 里没有 [$VER] 这一节，先补上（Release 说明不能是空的）"
fi
NOTES=$DIST/release-notes.md
awk -v ver="$VER" '
  $0 ~ "^## \\[" ver "\\]" { grab = 1; next }
  grab && /^## \[/ { grab = 0 }
  grab { print }
' CHANGELOG.md > "$NOTES.body"
{
  printf 'gojob %s —— 每个附件是一个平台的完整包（内含 gojob-server / gojob-demo / gojob-agent 与 README、CHANGELOG）。\n\n' "$TAG"
  printf '```sh\n'
  printf 'tar xzf gojob-%s-linux-amd64.tar.gz && cd gojob-%s-linux-amd64\n' "$VER" "$VER"
  printf './gojob-server            # sqlite 零依赖起服务，控制台 http://localhost:7800\n'
  printf 'shasum -a 256 -c checksums.txt   # 校验和覆盖 5 个平台包\n'
  printf '```\n\n'
  cat "$NOTES.body"
} > "$NOTES"
rm -f "$NOTES.body"
printf '   取自 CHANGELOG.md 的 [%s] 一节，共 %s 行\n' "$VER" "$(wc -l < "$NOTES" | tr -d ' ')"

if [ "$DRY" = 1 ]; then
  say "dry-run 结束"
  printf '   将要执行：git tag -a %s && git push origin HEAD && git push origin %s\n' "$TAG" "$TAG"
  printf '             gh release create %s --title "gojob %s" --notes-file dist/release-notes.md dist/gojob-*.tar.gz dist/checksums.txt\n' "$TAG" "$TAG"
  printf '   产物已在 %s/（未推送任何东西）\n' "$DIST"
  exit 0
fi

say "5. 打标签并推送"
git tag -a "$TAG" -m "gojob $TAG"
git push origin HEAD
git push origin "$TAG"

say "6. GitHub Release"
gh release create "$TAG" --title "gojob $TAG" --notes-file "$NOTES" \
  "$DIST"/gojob-*.tar.gz "$DIST"/checksums.txt
gh release view "$TAG" --json url -q .url
