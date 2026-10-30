#!/usr/bin/env bash
# 把上游 oci-core 合进 dev 的标准流程，严格两步：
#
#   1. main 同步上游（fast-forward，不产生合并提交）
#   2. dev 合并 main
#
# 这个脚本存在的唯一理由：合并上游有几步「忘了就出事」的动作——
# 推进 main、重建前端产物、跑测试。手工做总会漏掉其中一步，
# 而漏掉重建 dist 的表现是「编译通过、界面错位」，很难第一眼发现。
#
# 用法：
#   scripts/merge-upstream.sh                 # main 到 upstream/main，再合进 dev
#   scripts/merge-upstream.sh --until v0.5.0  # main 只到 v0.5.0，再合进 dev
#   scripts/merge-upstream.sh --check         # 只看会冲突哪些文件，不改工作区
#
# --until 用于分段推进：上游一次跨太多版本时，先合到某个 tag，确认没问题再继续。
# 注意它推进的是 main 本身，所以 main 与 dev 始终一致，不会出现
# 「main 在 v0.5.4、dev 只到 v0.5.0」这种错位。
#
# 冲突时脚本会停下并列出需要人工处理的文件，不会自作主张解决。

set -euo pipefail

cd "$(dirname "$0")/.."

RED=$'\033[31m'; YEL=$'\033[33m'; GRN=$'\033[32m'; DIM=$'\033[2m'; RST=$'\033[0m'
say()  { printf '%s\n' "$*"; }
ok()   { printf '%s✓%s %s\n' "$GRN" "$RST" "$*"; }
warn() { printf '%s!%s %s\n' "$YEL" "$RST" "$*"; }
die()  { printf '%s✗%s %s\n' "$RED" "$RST" "$*" >&2; exit 1; }

CHECK_ONLY=0
UNTIL=""
while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK_ONLY=1; shift ;;
    --until) [ $# -ge 2 ] || die "--until 需要一个 tag 或提交"; UNTIL="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "未知参数：$1（用法见 --help）" ;;
  esac
done

# ---------- 前置检查 ----------

git rev-parse --git-dir >/dev/null 2>&1 || die "当前目录不是 git 仓库"

if ! git remote get-url upstream >/dev/null 2>&1; then
  die "没有 upstream 远程。先执行：
    git remote add upstream git@github.com:jsongmax/oci-core.git"
fi

# merge=ours 没配的话，internal/web/dist 会刷出几十条 chunk 哈希冲突，
# 把真正的源码冲突埋掉。这里提前拦，而不是等用户面对一屏噪音。
if [ "$(git config --get merge.ours.driver || true)" != "true" ]; then
  die "没配 ours 合并驱动，dist 会产生大量无意义冲突。先执行：
    git config merge.ours.driver true"
fi

if [ "$CHECK_ONLY" -eq 0 ]; then
  if [ -n "$(git status --porcelain)" ]; then
    die "工作区不干净，先提交或 stash。当前改动：
$(git status --short | head -20)"
  fi
  CURRENT=$(git rev-parse --abbrev-ref HEAD)
  if [ "$CURRENT" != "dev" ]; then
    die "当前在 $CURRENT 上。合并上游只能在 dev 上做（main 要保持上游镜像）"
  fi
fi

# ---------- 拉取 ----------

say "拉取上游…"
git fetch upstream --tags --quiet
ok "已拉取"

UPSTREAM_REF="${UNTIL:-upstream/main}"
if [ -n "$UNTIL" ] && ! git rev-parse -q --verify "${UNTIL}^{commit}" >/dev/null; then
  die "找不到 $UNTIL（不是 tag 也不是提交）"
fi

say ""
say "待合入的上游提交："
git --no-pager log --oneline "dev..${UPSTREAM_REF}" | sed 's/^/  /'
COUNT=$(git rev-list --count "dev..${UPSTREAM_REF}")
[ "$COUNT" -eq 0 ] && { ok "dev 已是最新，无需合并"; exit 0; }

# ---------- 只检查 ----------

if [ "$CHECK_ONLY" -eq 1 ]; then
  say ""
  say "预演冲突（不改工作区）…"
  CONFLICTS=$(git merge-tree --write-tree --name-only dev "$UPSTREAM_REF" 2>&1 \
    | grep '^CONFLICT' \
    | sed -e 's/^CONFLICT ([^)]*): //' -e 's/^Merge conflict in //' \
    | sort -u || true)
  if [ -z "$CONFLICTS" ]; then
    ok "无冲突，可以直接合并"
    exit 0
  fi
  SRC=$(printf '%s\n' "$CONFLICTS" | grep -v '^internal/web/dist/' || true)
  DISTN=$(printf '%s\n' "$CONFLICTS" | grep -c '^internal/web/dist/' || true)
  say ""
  say "${YEL}源码层冲突 $([ -n "$SRC" ] && printf '%s\n' "$SRC" | wc -l || echo 0) 个：${RST}"
  [ -n "$SRC" ] && printf '%s\n' "$SRC" | sed 's/^/  /'
  say "${DIM}（另有 $DISTN 条 dist 冲突，合并时由 merge=ours 自动跳过）${RST}"
  exit 0
fi

# ---------- 第 1 步：main 同步上游 ----------

say ""
say "第 1 步：main 同步上游 → ${UPSTREAM_REF}"
git checkout --quiet main

# --until 指定的点必须是 main 的后代；否则是往回退，不能 fast-forward。
if ! git merge-base --is-ancestor main "$UPSTREAM_REF"; then
  git checkout --quiet dev
  die "main 已经越过 ${UPSTREAM_REF}，--until 只能往前推"
fi

if ! git merge --ff-only "$UPSTREAM_REF" --quiet 2>/dev/null; then
  git checkout --quiet dev
  die "main 无法 fast-forward，说明 main 上有本地提交。先把它们挪到 dev：
    git checkout main && git log --oneline upstream/main..main
    git checkout dev && git cherry-pick <那些提交> && git checkout main && git reset --hard upstream/main"
fi
ok "main 已到 $(git rev-parse --short HEAD) $(git log -1 --format=%s)"

# ---------- 第 2 步：dev 合并 main ----------

say ""
say "第 2 步：dev 合并 main"
git checkout --quiet dev
if ! git merge --no-ff main -m "merge: main 合入 dev（上游 ${UPSTREAM_REF}）"; then
  say ""
  warn "有冲突，脚本停在这里。工作区保留冲突现场。"
  say ""
  SRC=$(git diff --name-only --diff-filter=U | grep -v '^internal/web/dist/' || true)
  if [ -n "$SRC" ]; then
    say "${YEL}需要人工解决：${RST}"
    printf '%s\n' "$SRC" | sed 's/^/  /'
  fi
  say ""
  say "解决完之后："
  say "  git add -A && git commit"
  say "  rm -rf internal/web/dist && (cd web && npx vite build --emptyOutDir false)"
  say "  go build ./... && go vet ./... && go test ./..."
  say ""
  say "放弃这次合并：git merge --abort"
  exit 1
fi
ok "已合并"

# ---------- 重建前端产物 ----------

say ""
say "重建前端产物…"
if [ -d web/node_modules ]; then
  rm -rf internal/web/dist
  ( cd web && npx vite build --emptyOutDir false >/dev/null 2>&1 ) \
    || die "前端构建失败，先单独跑一次看报错：cd web && npx vite build --emptyOutDir false"
  ok "dist 已重建"
else
  warn "web/node_modules 不存在，跳过重建。dist 现在可能与 web/src 不一致，先执行：
    cd web && npm install --no-audit --no-fund --no-package-lock"
fi

# ---------- 验证 ----------

say ""
say "构建与测试…"
export PATH="$PWD/_tools/go/bin:$PATH"
export GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
go build ./... || die "go build 失败"
go vet ./...   || die "go vet 失败"
go test ./...  >/dev/null 2>&1 || die "go test 失败，单独跑一次看哪个包：
    go test ./..."
ok "全部通过"

# ---------- 收尾 ----------

# 合并提交本身已经由 --no-ff 建好了；这里要提交的是重建出来的 dist。
say ""
if [ -n "$(git status --porcelain)" ]; then
  say "dist 重建后有改动，单独提交一笔（与合并提交分开，便于二分定位）："
  say "  git add internal/web/dist && git commit -m \"chore: 重建前端产物\""
else
  ok "工作区干净，dist 无需更新"
fi
say ""
say "检查 diff 后推送："
say "  git log --oneline -3"
say "  git push origin dev"
