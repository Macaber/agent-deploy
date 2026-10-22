#!/usr/bin/env bash
# =============================================================================
# migrate-localpath-to-oss.sh
#
# 用途：把 local-path 本地磁盘上的 Workspace PVC 数据迁移到 OSS。
#
# 目录对应关系：
#   本地: /data/local-path-storage/pvc-<uid>_<ns>_ws-<ID>-pvc/{workspace,bocomwork,bocomcode}
#   OSS : oss://<bucket>/workspaces/ws-<ID>/{workspace,bocomwork,bocomcode}
#
# 每个工作空间执行的动作：
#   cd <本地PVC目录>
#   ossutil cp -r -f workspace  oss://<bucket>/workspaces/ws-<ID>/workspace/
#   ossutil cp -r -f bocomwork oss://<bucket>/workspaces/ws-<ID>/bocomwork/
#   ossutil cp -r -f bocomcode oss://<bucket>/workspaces/ws-<ID>/bocomcode/
#
# 同步语义（默认）：
#   先探测 oss://<bucket>/workspaces/ws-<ID>/ 下是否已有对象；
#   - 已有数据 -> 整个工作空间跳过，一个字节都不上传，不覆盖、不删除；
#   - 远端为空 -> 正常上传（OSS 上没有对应对照物时不存在覆盖问题）；
#   - 无法确认（ossutil 访问异常）-> 判失败并终止该工作空间，绝不盲目上传。
#   加 --force 才会忽略远端已有数据、强制覆盖上传。
#
# 注意：判断"有数据"时会忽略以 / 结尾的目录占位对象（0 字节 Key）。
#
# 常用示例：
#   bash migrate-localpath-to-oss.sh --list                  # 只列出本地已有的 ID
#   bash migrate-localpath-to-oss.sh --all --yes             # 全量迁移
#   bash migrate-localpath-to-oss.sh -id 9660946,08,01       # 指定 ID，不存在的自动跳过
#   bash migrate-localpath-to-oss.sh -f ids.txt              # 从文件读取 ID 列表
#   bash migrate-localpath-to-oss.sh --all --limit 5 --yes   # 只迁前 5 个（按 ID 排序）
#   bash migrate-localpath-to-oss.sh -id 9660946 --dry-run   # 演练：只打印命令
#   bash migrate-localpath-to-oss.sh --all --skip-done --yes # 断点续跑，跳过日志中已成功的
# =============================================================================

set -u
set -o pipefail

SCRIPT_NAME=${0##*/}
SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" >/dev/null 2>&1 && pwd -P)

# ------------------------------------------------------------------ 默认值
ROOT=${LP_ROOT:-}
ROOT_EXPLICIT=0
[ -n "${LP_ROOT:-}" ] && ROOT_EXPLICIT=1
OSS_BUCKET=${OSS_BUCKET:-oss://euvduatc-oss}
OSS_PREFIX=${OSS_PREFIX:-workspaces}
DIRS_CSV=${OSS_MIGRATE_DIRS:-workspace,bocomwork,bocomcode}
LOG_FILE=${OSS_MIGRATE_LOG:-}
LOG_EXPLICIT=0
[ -n "${OSS_MIGRATE_LOG:-}" ] && LOG_EXPLICIT=1
EXTRA_ARGS=${OSSUTIL_EXTRA_ARGS:-}
CP_FORCE_ARGS=${OSSUTIL_CP_FORCE_ARGS:--f}
RETRIES=${OSS_MIGRATE_RETRIES:-3}
RETRY_SLEEP=${OSS_MIGRATE_RETRY_SLEEP:-5}

MODE=""            # all | ids
RAW_IDS=""
IDS_FILES=()
SEEN_ALL=0
SEEN_IDS=0
LIMIT=0
DO_VERIFY=1
DO_STATS=1
VERBOSE=0
DRY_RUN=0
ASSUME_YES=0
SKIP_DONE=0
FORCE_UPLOAD=0
LIST_ONLY=0

# ------------------------------------------------------------------ 输出
if [ -t 1 ]; then
    C_INFO=$(printf '\033[36m'); C_OK=$(printf '\033[32m'); C_WARN=$(printf '\033[33m')
    C_ERR=$(printf '\033[31m'); C_DIM=$(printf '\033[2m'); C_END=$(printf '\033[0m')
else
    C_INFO=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_END=""
fi

ts()   { date '+%Y-%m-%dT%H:%M:%S%z'; }
info() { printf '%s[INFO]%s %s\n' "$C_INFO" "$C_END" "$*"; }
note() { printf '%s[ .. ]%s %s\n' "$C_DIM" "$C_END" "$*"; }
ok()   { printf '%s[ OK ]%s %s\n' "$C_OK" "$C_END" "$*"; }
warn() { printf '%s[WARN]%s %s\n' "$C_WARN" "$C_END" "$*" >&2; }
err()  { printf '%s[FAIL]%s %s\n' "$C_ERR" "$C_END" "$*" >&2; }
die()  { err "$*"; exit 2; }

usage() {
    cat <<'EOF'
用法:
  migrate-localpath-to-oss.sh --all [选项]
  migrate-localpath-to-oss.sh -id 9660946,08,01 [选项]
  migrate-localpath-to-oss.sh -f ids.txt [选项]
  migrate-localpath-to-oss.sh --list [选项]

迁移范围:
  --all               迁移本地全部 Workspace
  -id, --id LIST      只迁移指定 ID（逗号或空格分隔）；本地不存在的 ID 自动跳过、不做任何操作
                      支持简写: -id 9660946,08,01 等价于 ws-9660946 / ws-08 / ws-01
                      也支持 ws-9660946-pvc 写法；带随机后缀的 PVC（ws-9743616-guwpb）可用 9743616 唯一前缀匹配
  -f, --file FILE     从文件读取 ID；可与 -id 混用（取并集），也可 -f 多次指定多个文件
                      格式：每行一个 ID，或一行用逗号/空格/Tab/全角逗号分隔多个；
                      # 之后为注释；忽略空行、BOM、Windows 换行；重复 ID 自动去重；
                      ID 可写 9660946 / ws-9660946 / ws-9660946-pvc，
                      也可直接粘 pvc-<uuid>_<ns>_ws-<ID>-pvc 目录名或完整路径；
                      FILE 为 - 时从标准输入读取
  --limit N           最多迁移 N 个（按 ID 排序；可配合 --all / -id / -f 使用，实现分批迁移）
  --list              仅列出本地已有的 Workspace ID 及目录，不做任何迁移（不需要 ossutil）

数据源:
  --root DIR          local-path 数据根目录；支持相对路径
                      未指定时自动识别：当前目录下若有 pvc-*_<ns>_ws-<ID>-pvc
                      （即已 cd 进数据目录）就用当前目录，否则用 /data/local-path-storage
  --dirs LIST         需要迁移的子目录，逗号分隔（默认: workspace,bocomwork,bocomcode）

目标:
  --bucket BUCKET     OSS Bucket，以 oss:// 开头（默认: oss://euvduatc-oss）
  --prefix PREFIX     Bucket 内的前缀（默认: workspaces）
                      最终路径: oss://<bucket>/<prefix>/ws-<ID>/

执行控制:
  同步语义            远端已有数据则跳过整个工作空间；远端为空才上传
  --force             忽略远端已有数据，强制覆盖上传（默认不做任何覆盖）
  --no-verify         上传后不做远端对象数量抽查
  --no-stats          不统计本地大小/文件数（超大目录可加快速度）
  -v, --verbose       实时打印 ossutil 原始输出（默认只打印摘要，失败时回放错误）
  --dry-run           演练：只打印将要执行的 ossutil 命令，不删除、不上传、不写日志
  --skip-done         跳过日志中已成功（OK）的工作空间，便于断点续跑
  --retries N         单个 ossutil 操作失败后的最大尝试次数（默认 3）
  --ossutil-arg ARG   追加透传给 ossutil 的参数（如 --ossutil-arg=--jobs=10），可重复
  -y, --yes           跳过交互确认（非交互环境如 cron 必须加）
  -h, --help          显示本帮助

日志:
  --log FILE          迁移日志（默认: <脚本目录>/logs/oss-migrate.log）
                      TSV 列: time  status  workspace  local_dir  remote_prefix  dirs  size  files  note
                      只有 status=OK 表示该工作空间全部子目录上传成功

环境变量: LP_ROOT / OSS_BUCKET / OSS_PREFIX / OSS_MIGRATE_DIRS / OSS_MIGRATE_LOG
          OSS_MIGRATE_RETRIES / OSS_MIGRATE_RETRY_SLEEP / OSSUTIL_EXTRA_ARGS
          OSSUTIL_CP_FORCE_ARGS（默认 -f，即强制覆盖已存在对象）

退出码: 0=全部成功（含跳过）  1=存在失败  2=参数或环境错误
EOF
}

# ------------------------------------------------------------------ 日志
LOG_READY=0

log_init() {
    local dir alt
    if [ -z "$LOG_FILE" ]; then
        LOG_FILE="$SCRIPT_DIR/logs/oss-migrate.log"
    fi
    dir=$(dirname -- "$LOG_FILE")

    if ! mkdir -p -- "$dir" 2>/dev/null || [ ! -w "$dir" ]; then
        # 用户显式指定的路径不可写 -> 直接报错，避免把日志写到意料之外的位置
        [ "$LOG_EXPLICIT" -eq 1 ] && die "日志目录不可写: ${dir}（请换一个 --log 路径）"
        alt="${HOME:-/tmp}/.oss-migrate"
        mkdir -p -- "$alt" 2>/dev/null || die "无法创建日志目录: ${dir}（可用 --log 指定可写路径）"
        LOG_FILE="$alt/$(basename -- "$LOG_FILE")"
        dir=$alt
        warn "脚本目录不可写，日志已改写到: ${LOG_FILE}"
    fi

    if [ ! -s "$LOG_FILE" ]; then
        printf '# time\tstatus\tworkspace\tlocal_dir\tremote_prefix\tdirs\tsize\tfiles\tnote\n' >>"$LOG_FILE" \
            || die "无法写入日志文件: ${LOG_FILE}"
    fi
    LOG_READY=1
}

# log_write STATUS WORKSPACE LOCAL_DIR REMOTE_PREFIX DIRS SIZE FILES [NOTE]
log_write() {
    [ "$LOG_READY" -eq 1 ] || return 0
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
        "$(ts)" "$1" "$2" "$3" "$4" "$5" "$6" "$7" "${8:-}" >>"$LOG_FILE" \
        || warn "写日志失败: ${LOG_FILE}"
}

# 日志中该工作空间是否已成功过
log_has_ok() {
    [ -s "$LOG_FILE" ] || return 1
    awk -F'\t' -v ws="$1" '$2 == "OK" && $3 == ws { hit = 1 } END { exit(hit ? 0 : 1) }' "$LOG_FILE"
}

# ------------------------------------------------------------------ 小工具
valid_id() {
    case "$1" in
        ''|*[!A-Za-z0-9._-]*) return 1 ;;
        *) return 0 ;;
    esac
}

# 统一成纯 ID，容忍以下写法：
#   9660946 / ws-9660946 / ws-9660946-pvc
#   pvc-<uuid>_bocomwork_ws-9660946-pvc          （直接粘 ls 结果）
#   /data/local-path-storage/pvc-..._ws-08-pvc/  （直接粘完整路径）
normalize_id() {
    local id=$1
    id=${id%/}
    id=${id##*/}
    case "$id" in
        *_ws-*) id=${id##*_ws-} ;;
    esac
    id=${id#ws-}
    id=${id%-pvc}
    printf '%s' "$id"
}

num_norm() {
    local v
    v=$(printf '%s' "$1" | sed 's/^0*//')
    printf '%s' "${v:-0}"
}

# 目录名 -> ID:  pvc-xxx_bocomwork_ws-9660946-pvc -> 9660946
dir_to_id() {
    local base=${1##*/}
    local id=${base##*_ws-}
    printf '%s' "${id%-pvc}"
}

local_ids() {
    local d
    for d in "$ROOT"/pvc-*_*_ws-*-pvc; do
        [ -d "$d" ] || continue
        dir_to_id "$d"
        printf '\n'
    done
}

human_size() {
    awk -v k="${1:-0}" 'BEGIN {
        if (k < 1024) { printf "%dK", k; exit }
        n = split("K M G T P", u, " "); v = k; i = 1
        while (v >= 1024 && i < n) { v /= 1024; i++ }
        printf "%.1f%s", v, u[i]
    }'
}

csv() {
    local IFS=,
    printf '%s' "$*"
}

ossutil_run() {
    # 透传参数放在子命令之后（ossutil 1.x 只接受 "<subcmd> [options] ..." 的顺序）
    local sub=$1; shift
    # shellcheck disable=SC2086
    ossutil "$sub" $EXTRA_ARGS "$@"
}

# ------------------------------------------------------------------ ID 收集
# 从标准输入解析 ID：一行可写多个（逗号或空格分隔）
#   * 忽略空行、只有空白字符的行
#   * # 之后视为注释
#   * 兼容 Windows 换行（行尾 CR）
#   * 兼容 ws- 前缀 / -pvc 后缀（由 normalize_id 统一处理）
read_id_lines() {
    local line p
    local -a parts=()
    while IFS= read -r line || [ -n "$line" ]; do
        line=${line%$'\r'}          # Windows 换行
        line=${line#$'\xef\xbb\xbf'}   # UTF-8 BOM（记事本另存为 UTF-8 时会带）
        line=${line%%#*}              # # 之后是注释
        line=${line//，/,}            # 中文全角逗号也当分隔符
        case "$line" in
            *[![:space:]]*) ;;
            *) continue ;;
        esac
        # 逗号、空格、Tab 都是分隔符
        IFS=$', \t' read -r -a parts <<< "$line" || true
        for p in "${parts[@]}"; do
            [ -n "$p" ] && printf '%s\n' "$p"
        done
    done
}

# 输出 -id 与 -f 提供的全部原始 ID（每行一个）
collect_raw_ids() {
    local f p
    local -a parts=()

    if [ -n "$RAW_IDS" ]; then
        RAW_IDS=${RAW_IDS//，/,}
        IFS=$', \t' read -r -a parts <<< "$RAW_IDS" || true
        for p in "${parts[@]}"; do
            [ -n "$p" ] && printf '%s\n' "$p"
        done
    fi

    [ "${#IDS_FILES[@]}" -gt 0 ] || return 0

    for f in "${IDS_FILES[@]}"; do
        if [ "$f" = "-" ]; then
            read_id_lines
        else
            read_id_lines < "$f"
        fi
    done
}

# ------------------------------------------------------------------ ID 解析
RESOLVE_DIR=""
RESOLVE_ID=""
RESOLVE_NOTE=""
RESOLVE_CANDS=""

# resolve_target ID
#   0 = 唯一命中（RESOLVE_DIR / RESOLVE_ID 有效）
#   1 = 本地不存在
#   2 = 命中多个（RESOLVE_NOTE / RESOLVE_CANDS 说明歧义）
resolve_target() {
    local id=$1 d base cand cnt want

    RESOLVE_DIR=""; RESOLVE_ID=$id; RESOLVE_NOTE=""; RESOLVE_CANDS=""

    # 1) 精确匹配 ws-<id>-pvc
    cnt=0
    for d in "$ROOT"/pvc-*_*_ws-"$id"-pvc; do
        [ -d "$d" ] || continue
        cnt=$((cnt + 1)); RESOLVE_DIR=$d
    done
    if [ "$cnt" -eq 1 ]; then return 0; fi
    if [ "$cnt" -gt 1 ]; then RESOLVE_NOTE="本地存在多个同名目录"; return 2; fi

    # 2) 前缀匹配 ws-<id>-xxx-pvc（带随机后缀的 PVC，如 ws-9743616-guwpb）
    cnt=0
    while IFS= read -r cand; do
        [ -n "$cand" ] || continue
        case "$cand" in
            "$id"-*)
                cnt=$((cnt + 1))
                RESOLVE_CANDS="${RESOLVE_CANDS} ws-${cand}"
                for d in "$ROOT"/pvc-*_*_ws-"$cand"-pvc; do
                    [ -d "$d" ] || continue
                    RESOLVE_ID=$cand; RESOLVE_DIR=$d
                done
                ;;
        esac
    done < <(local_ids | sort -u)
    if [ "$cnt" -eq 1 ]; then
        RESOLVE_NOTE="按前缀匹配到 ws-${RESOLVE_ID}"
        return 0
    fi
    if [ "$cnt" -gt 1 ]; then
        RESOLVE_NOTE="前缀匹配到多个工作空间，请写完整 ID"
        return 2
    fi

    # 3) 数字归一化匹配（1 -> ws-01 / ws-0001，必须唯一）
    case "$id" in
        *[!0-9]*) return 1 ;;
    esac
    want=$(num_norm "$id")
    cnt=0
    RESOLVE_CANDS=""
    while IFS= read -r cand; do
        [ -n "$cand" ] || continue
        case "$cand" in *[!0-9]*) continue ;; esac
        if [ "$(num_norm "$cand")" = "$want" ]; then
            cnt=$((cnt + 1))
            RESOLVE_CANDS="${RESOLVE_CANDS} ws-${cand}"
            RESOLVE_ID=$cand
            for d in "$ROOT"/pvc-*_*_ws-"$cand"-pvc; do
                [ -d "$d" ] || continue
                RESOLVE_DIR=$d
            done
        fi
    done < <(local_ids | sort -u)
    if [ "$cnt" -eq 1 ] && [ -n "$RESOLVE_DIR" ]; then
        RESOLVE_NOTE="按数字编号匹配到 ws-${RESOLVE_ID}"
        return 0
    fi
    if [ "$cnt" -gt 1 ]; then
        RESOLVE_NOTE="数字编号匹配到多个工作空间，请写完整 ID"
        return 2
    fi
    return 1
}

# ------------------------------------------------------------------ 参数解析
while [ $# -gt 0 ]; do
    case "$1" in
        --all)              MODE=all; SEEN_ALL=1 ;;
        -id|--id|--ids)
            [ $# -ge 2 ] || die "$1 需要一个参数，例如 -id 9660946,08,01"
            RAW_IDS=$2; MODE=ids; SEEN_IDS=1; shift ;;
        -id=*|--id=*|--ids=*) RAW_IDS=${1#*=}; MODE=ids; SEEN_IDS=1 ;;
        -f|--file|--ids-file)
            [ $# -ge 2 ] || die "$1 需要一个文件路径（- 表示从标准输入读取）"
            IDS_FILES[${#IDS_FILES[@]}]=$2; MODE=ids; SEEN_IDS=1; shift ;;
        -f=*|--file=*|--ids-file=*) IDS_FILES[${#IDS_FILES[@]}]=${1#*=}; MODE=ids; SEEN_IDS=1 ;;
        --limit)
            [ $# -ge 2 ] || die "--limit 需要一个数字参数"; LIMIT=$2; shift ;;
        --limit=*)          LIMIT=${1#*=} ;;
        --root)
            [ $# -ge 2 ] || die "--root 需要一个参数"; ROOT=$2; ROOT_EXPLICIT=1; shift ;;
        --root=*)           ROOT=${1#*=}; ROOT_EXPLICIT=1 ;;
        --bucket)
            [ $# -ge 2 ] || die "--bucket 需要一个参数"; OSS_BUCKET=$2; shift ;;
        --bucket=*)         OSS_BUCKET=${1#*=} ;;
        --prefix)
            [ $# -ge 2 ] || die "--prefix 需要一个参数"; OSS_PREFIX=$2; shift ;;
        --prefix=*)         OSS_PREFIX=${1#*=} ;;
        --dirs)
            [ $# -ge 2 ] || die "--dirs 需要一个参数"; DIRS_CSV=$2; shift ;;
        --dirs=*)           DIRS_CSV=${1#*=} ;;
        --log)
            [ $# -ge 2 ] || die "--log 需要一个参数"; LOG_FILE=$2; LOG_EXPLICIT=1; shift ;;
        --log=*)            LOG_FILE=${1#*=}; LOG_EXPLICIT=1 ;;
        --retries)
            [ $# -ge 2 ] || die "--retries 需要一个数字参数"; RETRIES=$2; shift ;;
        --retries=*)        RETRIES=${1#*=} ;;
        --ossutil-arg)
            [ $# -ge 2 ] || die "--ossutil-arg 需要一个参数"; EXTRA_ARGS="$EXTRA_ARGS $2"; shift ;;
        --ossutil-arg=*)    EXTRA_ARGS="$EXTRA_ARGS ${1#*=}" ;;
        --no-verify)        DO_VERIFY=0 ;;
        --no-stats)         DO_STATS=0 ;;
        -v|--verbose)       VERBOSE=1 ;;
        --dry-run)          DRY_RUN=1 ;;
        --skip-done)        SKIP_DONE=1 ;;
        --force)            FORCE_UPLOAD=1 ;;
        -y|--yes)           ASSUME_YES=1 ;;
        --list)             LIST_ONLY=1 ;;
        -h|--help)          usage; exit 0 ;;
        *)                  die "未知参数: $1（使用 --help 查看用法）" ;;
    esac
    shift
done

case "$LIMIT" in ''|*[!0-9]*) die "--limit 必须是非负整数: ${LIMIT}" ;; esac
case "$RETRIES" in ''|*[!0-9]*|0) die "--retries 必须是正整数: ${RETRIES}" ;; esac

[ "$SEEN_ALL" -eq 1 ] && [ "$SEEN_IDS" -eq 1 ] && die "--all 与 -id/-f 不能同时使用"

# -id / -f 传了但没有任何有效条目，视为用法错误
if [ "$MODE" = ids ] && [ -z "$(printf '%s' "$RAW_IDS" | tr -d ' ,')" ] && [ "${#IDS_FILES[@]}" -eq 0 ]; then
    die "请用 -id <ID列表> 或 -f <ID文件> 指定要迁移的工作空间"
fi

# ------------------------------------------------------------------ 预检
# 未显式指定 --root / LP_ROOT 时自动定位数据目录：
#   1) 当前目录下就有 pvc-*_..._ws-*-pvc（说明已 cd 进数据目录）-> 用当前目录
#   2) 否则若 /data/local-path-storage 存在 -> 用它
#   3) 都没有 -> 报错并提示用 --root 指定
DETECT_HINT=0

detect_root() {
    [ "$ROOT_EXPLICIT" -eq 1 ] && return 0
    local d

    for d in "$PWD"/pvc-*_*_ws-*-pvc; do
        if [ -d "$d" ]; then
            ROOT=$PWD
            note "未指定 --root，自动使用当前目录作为数据根目录: ${ROOT}"
            return 0
        fi
    done

    if [ -d /data/local-path-storage ]; then
        ROOT=/data/local-path-storage
        return 0
    fi

    ROOT=/data/local-path-storage
    DETECT_HINT=1
}

# 相对路径转绝对路径 + 存在性校验
resolve_root() {
    case "$ROOT" in
        /*) ;;
        *)
            local abs
            abs=$(cd -- "$ROOT" 2>/dev/null && pwd -P) || die "--root 目录不存在: ${ROOT}"
            ROOT=$abs
            ;;
    esac
    if [ ! -d "$ROOT" ]; then
        if [ "$DETECT_HINT" -eq 1 ]; then
            die "未找到 local-path 数据目录：${ROOT} 不存在，当前目录 ${PWD} 下也没有 pvc-*_<ns>_ws-<ID>-pvc，请用 --root 指定"
        fi
        die "本地数据根目录不存在: ${ROOT}"
    fi
}

preflight() {
    resolve_root

    case "$OSS_BUCKET" in
        oss://*) ;;
        *) die "--bucket 必须以 oss:// 开头: ${OSS_BUCKET}" ;;
    esac
    OSS_BUCKET=${OSS_BUCKET%/}

    OSS_PREFIX=${OSS_PREFIX#/}
    OSS_PREFIX=${OSS_PREFIX%/}
    [ -n "$OSS_PREFIX" ] || die "--prefix 不能为空"

    DIR_LIST=$(printf '%s' "$DIRS_CSV" | tr ',' ' ')
    DIR_LIST=$(printf '%s' "$DIR_LIST" | tr -s '[:space:]' ' ')
    DIR_LIST=${DIR_LIST# }
    DIR_LIST=${DIR_LIST% }
    [ -n "$DIR_LIST" ] || die "--dirs 不能为空"

    local d
    for d in $DIR_LIST; do
        case "$d" in
            *[!A-Za-z0-9._-]*) die "--dirs 中含有非法目录名: ${d}" ;;
            */*) die "--dirs 不支持带路径的目录: ${d}" ;;
        esac
    done

    if [ "$DRY_RUN" -eq 0 ] && ! command -v ossutil >/dev/null 2>&1; then
        die "未找到 ossutil 命令，请先安装并执行 ossutil config 配置 AK"
    fi
}

# ------------------------------------------------------------------ 目标集合
TARGET_IDS=()
TARGET_DIRS=()
CNT_OK=0
CNT_FAIL=0
CNT_SKIP=0
FAILED_IDS=()

push_target() {
    local i
    if [ "${#TARGET_IDS[@]}" -gt 0 ]; then
        for i in "${TARGET_IDS[@]}"; do
            [ "$i" = "$1" ] && return 0
        done
    fi
    TARGET_IDS[${#TARGET_IDS[@]}]=$1
    TARGET_DIRS[${#TARGET_DIRS[@]}]=$2
}

build_targets() {
    local raw id rc

    if [ "$MODE" = ids ]; then
        # 文件先校验（必须在主 shell 里，die 才能终止整个脚本）
        if [ "${#IDS_FILES[@]}" -gt 0 ]; then
            local f
            for f in "${IDS_FILES[@]}"; do
                [ "$f" = "-" ] && continue
                [ -e "$f" ] || die "ID 文件不存在: ${f}"
                [ -f "$f" ] || die "ID 文件不是普通文件: ${f}"
                [ -r "$f" ] || die "ID 文件不可读: ${f}"
            done
        fi
        while IFS= read -r raw; do
            [ -n "$raw" ] || continue
            id=$(normalize_id "$raw")
            if ! valid_id "$id"; then
                err "非法 ID: ${raw}（只允许字母、数字、. _ -）"
                CNT_SKIP=$((CNT_SKIP + 1))
                continue
            fi
            resolve_target "$id"
            rc=$?
            case "$rc" in
                0)
                    if [ "$RESOLVE_ID" != "$id" ]; then
                        note "ID ${raw} -> ws-${RESOLVE_ID}（${RESOLVE_NOTE}）"
                    fi
                    push_target "$RESOLVE_ID" "$RESOLVE_DIR"
                    ;;
                1)
                    warn "ws-${id} 在 ${ROOT} 下不存在，跳过（不做任何操作）"
                    CNT_SKIP=$((CNT_SKIP + 1))
                    ;;
                *)
                    err "ws-${id} 无法唯一确定：${RESOLVE_NOTE}${RESOLVE_CANDS}，已跳过"
                    CNT_SKIP=$((CNT_SKIP + 1))
                    ;;
            esac
        done < <(collect_raw_ids)
        # 一个 ID 都没解析出来（空文件 / 全是注释）视为用法错误
        if [ "$CNT_SKIP" -eq 0 ] && [ "${#TARGET_IDS[@]}" -eq 0 ]; then
            die "-id/-f 没有提供任何有效 ID（文件为空，或内容全是注释）"
        fi
    else
        while IFS= read -r id; do
            [ -n "$id" ] || continue
            resolve_target "$id"
            case $? in
                0) push_target "$RESOLVE_ID" "$RESOLVE_DIR" ;;
                2)
                    warn "ws-${id} 无法唯一确定（${RESOLVE_NOTE}），--all 已跳过"
                    CNT_SKIP=$((CNT_SKIP + 1))
                    ;;
            esac
        done < <(local_ids | sort -u)
    fi

    [ "${#TARGET_IDS[@]}" -eq 0 ] && return 0

    if [ "$SKIP_DONE" -eq 1 ] && [ -s "$LOG_FILE" ]; then
        local i kept_ids=() kept_dirs=()
        for ((i = 0; i < ${#TARGET_IDS[@]}; i++)); do
            if log_has_ok "ws-${TARGET_IDS[$i]}"; then
                note "ws-${TARGET_IDS[$i]} 日志中已成功，跳过"
                CNT_SKIP=$((CNT_SKIP + 1))
            else
                kept_ids[${#kept_ids[@]}]=${TARGET_IDS[$i]}
                kept_dirs[${#kept_dirs[@]}]=${TARGET_DIRS[$i]}
            fi
        done
        if [ "${#kept_ids[@]}" -gt 0 ]; then
            TARGET_IDS=("${kept_ids[@]}")
            TARGET_DIRS=("${kept_dirs[@]}")
        else
            TARGET_IDS=()
            TARGET_DIRS=()
        fi
    fi

    if [ "$LIMIT" -gt 0 ] && [ "${#TARGET_IDS[@]}" -gt "$LIMIT" ]; then
        note "按 --limit ${LIMIT} 只处理前 ${LIMIT} 个"
        TARGET_IDS=("${TARGET_IDS[@]:0:$LIMIT}")
        TARGET_DIRS=("${TARGET_DIRS[@]:0:$LIMIT}")
    fi
}

print_plan() {
    local i
    printf '\n%s迁移计划%s\n' "$C_INFO" "$C_END"
    printf '  本地根目录 : %s\n' "$ROOT"
    printf '  目标前缀   : %s/%s/\n' "$OSS_BUCKET" "$OSS_PREFIX"
    printf '  迁移子目录 : %s\n' "$DIRS_CSV"
    if [ "$FORCE_UPLOAD" -eq 1 ]; then
        printf '  远端已有数据 : --force 已开启，忽略远端状态，直接覆盖上传\n'
    else
        printf '  远端已有数据 : 跳过整个工作空间（不上传、不覆盖、不删除）\n'
    fi
    printf '  待处理数量 : %s\n' "${#TARGET_IDS[@]}"
    if [ "$DRY_RUN" -eq 1 ]; then
        printf '  运行模式   : 演练（--dry-run，不会真的操作 OSS）\n'
    fi
    printf '\n'
    for ((i = 0; i < ${#TARGET_IDS[@]}; i++)); do
        printf '  %-18s <- %s\n' "ws-${TARGET_IDS[$i]}" "${TARGET_DIRS[$i]}"
    done
    printf '\n'
}

confirm() {
    [ "$DRY_RUN" -eq 1 ] && return 0
    [ "$ASSUME_YES" -eq 1 ] && return 0
    [ -t 0 ] || die "当前为非交互环境，请追加 --yes 确认后执行"
    printf '确认继续？[y/N] '
    local ans
    read -r ans || ans=""
    case "$ans" in
        y|Y|yes|YES) return 0 ;;
        *) info "已取消，未做任何操作"; exit 0 ;;
    esac
}

# ------------------------------------------------------------------ 单步执行
# run_ossutil_with_retry DESC ARGS...
# 将输出写入 LOG_TMP，返回 0=成功 1=失败
LOG_TMP=""

run_ossutil_with_retry() {
    local desc=$1; shift
    local attempt=1 rc=1

    while [ "$attempt" -le "$RETRIES" ]; do
        if [ "$VERBOSE" -eq 1 ]; then
            ossutil_run "$@" 2>&1 | tee "$LOG_TMP"
            rc=${PIPESTATUS[0]}
        else
            ossutil_run "$@" >"$LOG_TMP" 2>&1
            rc=$?
        fi
        [ "$rc" -eq 0 ] && return 0
        if [ "$attempt" -lt "$RETRIES" ]; then
            warn "${desc} 第 ${attempt}/${RETRIES} 次失败（rc=${rc}），${RETRY_SLEEP}s 后重试"
            sleep "$RETRY_SLEEP"
        fi
        attempt=$((attempt + 1))
    done

    # 全部重试失败：打印最后一次的错误输出，便于排障
    if [ -s "$LOG_TMP" ]; then
        tail -n 20 "$LOG_TMP" | sed 's/^/       /'
    fi
    return 1
}

# 判定远端前缀状态（结果通过返回值表达）
#   0 = 已有真实对象（有数据）
#   1 = 空（没有任何对象，或只有 "/" 结尾的目录占位对象）
#   2 = 无法确认（ossutil 报错且不是"不存在"类错误，如权限/网络问题）
# 说明：
#   * 只有目录占位对象不算数据，否则上一次中断遗留的空目录会让本次永远跳过；
#   * ossutil ls 的输出里对象名在行尾，形如 "... ETAG  oss://bucket/key"，
#     所以按"行内包含 oss:// 且行尾不是 /"判定。
remote_state() {
    local prefix=$1 rc out
    # 快速路径：只看前 200 行，出现真实对象立刻返回（head 提前关管道，ossutil 收到 SIGPIPE 即停止列举）
    out=$(ossutil_run ls "$prefix" 2>/dev/null | head -n 200)
    if printf '%s\n' "$out" | grep 'oss://' | grep -v '/$' >/dev/null 2>&1; then
        return 0
    fi
    # 前 200 行没看到真实对象：做一次完整判定，并区分"空/不存在"与"访问异常"
    ossutil_run ls "$prefix" >"$LOG_TMP" 2>&1
    rc=$?
    if [ "$rc" -ne 0 ]; then
        # 126/127 = 命令不可执行/不存在，属于"环境异常"，不能当成空
        [ "$rc" -eq 126 ] || [ "$rc" -eq 127 ] && return 2
        # 前缀/对象不存在不是错误，视为空；其余错误（权限、网络等）一律按"无法确认"处理
        case "$(cat "$LOG_TMP" 2>/dev/null)" in
            *NoSuchKey*|*NoSuchBucket*|*"no such key"*|*"not exist"*|*"does not exist"*|*404*)
                return 1 ;;
            *) return 2 ;;
        esac
    fi
    if awk '/oss:\/\// { if ($0 ~ /\/$/) next; f = 1; exit } END { exit(f ? 0 : 1) }' "$LOG_TMP" 2>/dev/null; then
        return 0
    fi
    return 1
}

# 远端前缀下是否还有对象
# 注意:
#   1) ossutil ls 的对象行形如 "...  Standard  ETAG=xxx  oss://bucket/key"，
#      对象名不在行首，所以按"行内是否包含 oss://"判定（汇总行 Object Number is: 不含 oss://）
#   2) 不能写成 `ossutil ls | grep -q`：pipefail 下 grep -q 提前退出会让 ossutil 收到 SIGPIPE(141)
remote_prefix_has_objects() {
    local out
    # head 提前关闭管道，ossutil 收到 SIGPIPE 即停止列举（pipefail 下忽略其退出码）
    out=$(ossutil_run ls "$1" 2>/dev/null | head -n 20)
    case "$out" in
        *oss://*) return 0 ;;
        *) return 1 ;;
    esac
}

# 统计远端对象数（最多取 CAP 行，超出则标记 +）
VERIFY_CAPPED=""
remote_count() {
    local out n cap=${2:-500}
    VERIFY_CAPPED=""
    out=$(ossutil_run ls "$1" 2>/dev/null | head -n "$((cap * 2))")
    n=$(printf '%s\n' "$out" | awk -v cap="$cap" '/oss:\/\// { c++; if (c >= cap) exit } END { print c + 0 }')
    n=${n:-0}
    [ "$n" -ge "$cap" ] && VERIFY_CAPPED="+"
    printf '%s' "$n"
}

# ------------------------------------------------------------------ 单个迁移
# 返回: 0=成功 1=失败 3=跳过
migrate_one() {
    local id=$1 dir=$2
    local remote="${OSS_BUCKET}/${OSS_PREFIX}/ws-${id}"
    local d present=() paths=() rc size files dirs_str failed_dir="" remote_cnt

    printf '\n%s==== ws-%s ====%s\n' "$C_INFO" "$id" "$C_END"
    note "本地: ${dir}"
    note "远端: ${remote}"

    if [ ! -r "$dir" ] || [ ! -x "$dir" ]; then
        err "ws-${id} 本地目录不可读: ${dir}（当前用户 $(id -un)，请检查权限或改用 sudo）"
        log_write FAIL "ws-${id}" "$dir" "$remote" "" "" "" "本地目录不可读"
        return 1
    fi

    for d in $DIR_LIST; do
        if [ -d "$dir/$d" ]; then
            present[${#present[@]}]=$d
        else
            note "本地缺少子目录 ${d}，跳过该子目录"
        fi
    done

    if [ "${#present[@]}" -eq 0 ]; then
        warn "ws-${id} 本地没有可迁移的子目录（${DIRS_CSV}），跳过"
        log_write SKIP "ws-${id}" "$dir" "$remote" "" "" "" "本地无目标子目录"
        return 3
    fi

    # 1) 远端已有数据则整ws跳过（默认不覆盖；--force 才强制上传）
    if [ "$FORCE_UPLOAD" -eq 0 ]; then
        remote_state "$remote/"
        case $? in
            0)
                if [ "$DRY_RUN" -eq 1 ]; then
                    note "[dry-run] 远端已有数据，将跳过: ${remote}"
                else
                    warn "ws-${id} 远端已有数据，跳过（不上传、不覆盖、不删除）: ${remote}"
                    log_write SKIP "ws-${id}" "$dir" "$remote" "$(csv "${present[@]}")" "" "" "远端已有数据，跳过"
                fi
                return 3
                ;;
            1)
                note "远端为空，可以上传"
                ;;
            *)
                if [ "$DRY_RUN" -eq 1 ]; then
                    warn "无法确认远端状态（ossutil 不可用或访问异常），演练继续"
                else
                    err "ws-${id} 无法确认远端 ${remote} 的状态，为避免覆盖已有数据，已终止该工作空间"
                    log_write FAIL "ws-${id}" "$dir" "$remote" "$(csv "${present[@]}")" "" "" "无法确认远端状态"
                    return 1
                fi
                ;;
        esac
    else
        note "已指定 --force，忽略远端已有数据，直接覆盖上传"
    fi

    # 2) 逐个子目录上传
    for d in "${present[@]}"; do
        if [ "$DRY_RUN" -eq 1 ]; then
            note "[dry-run] (cd ${dir} && ossutil cp -r ${CP_FORCE_ARGS} ${d} ${remote}/${d}/)"
            continue
        fi
        info "上传 ${d} -> ${remote}/${d}/（已存在则覆盖）"
        # shellcheck disable=SC2086
        if ! ( cd "$dir" && run_ossutil_with_retry "cp ${d}" cp -r $CP_FORCE_ARGS "$d" "$remote/$d/" ); then
            failed_dir=$d
            break
        fi
    done

    if [ -n "$failed_dir" ]; then
        err "ws-${id} 上传子目录 ${failed_dir} 失败（已重试 ${RETRIES} 次）"
        log_write FAIL "ws-${id}" "$dir" "$remote" "$(csv "${present[@]}")" "" "" "ossutil cp 失败: ${failed_dir}"
        return 1
    fi

    if [ "$DRY_RUN" -eq 1 ]; then
        note "[dry-run] 成功后将写入日志: ${LOG_FILE}"
        ok "ws-${id} 演练完成（未执行任何写操作）"
        return 0
    fi

    # 3) 远端抽查
    #    注意：不删除远端，所以历史对象会让"某目录非空"变得不可靠。
    #    这里只把"整个前缀一个对象都没有、但本地确有文件"判为失败；
    #    逐个目录的数量差异只作告警（符号链接、目录占位对象都会让计数不等）。
    dirs_str=$(csv "${present[@]}")
    remote_cnt=NA
    if [ "$DO_VERIFY" -eq 1 ]; then
        local local_files=0 d_lc warn_dirs=""
        for d in "${present[@]}"; do
            d_lc=$(find "$dir/$d" -type f 2>/dev/null | wc -l | tr -d ' ')
            local_files=$((local_files + ${d_lc:-0}))
            if [ "${d_lc:-0}" -gt 0 ] && ! remote_prefix_has_objects "$remote/$d/"; then
                warn_dirs="${warn_dirs} ${d}"
            fi
        done

        remote_cnt=$(remote_count "$remote/" 500)
        if [ "${remote_cnt:-0}" -eq 0 ]; then
            if [ "$local_files" -gt 0 ]; then
                err "ws-${id} 本地有 ${local_files} 个文件，但远端 ${remote} 下 1 个对象都没有，请人工确认"
                log_write FAIL "ws-${id}" "$dir" "$remote" "$dirs_str" "" "" "远端校验为空"
                return 1
            fi
            note "本地无文件，远端无对象，跳过校验"
        else
            [ -n "$warn_dirs" ] && warn "ws-${id} 以下子目录本地有文件但远端未见对象（可能被历史对象掩盖，也可能未上传）:${warn_dirs}"
            note "远端抽查通过（${remote_cnt}${VERIFY_CAPPED} 个对象）"
        fi
    fi

    # 4) 本地统计 + 成功日志
    for d in "${present[@]}"; do
        paths[${#paths[@]}]=$dir/$d
    done
    size="-"; files="-"
    if [ "$DO_STATS" -eq 1 ]; then
        size=$(human_size "$(du -sk "${paths[@]}" 2>/dev/null | awk '{s += $1} END {printf "%d", s + 0}')")
        files=$(find "${paths[@]}" -type f 2>/dev/null | wc -l | tr -d ' ')
    fi

    log_write OK "ws-${id}" "$dir" "$remote" "$dirs_str" "$size" "$files" "remote_objects=${remote_cnt}"
    ok "ws-${id} 迁移完成（${dirs_str}，${size}，${files} 个文件）"
    return 0
}

# ------------------------------------------------------------------ 主流程
preflight_root_only() {
    resolve_root
}

# --list：打印本地 Workspace ID 与目录
list_local() {
    local d base id cnt=0
    printf '本地工作空间（%s）:\n' "$ROOT"
    for d in "$ROOT"/pvc-*_*_ws-*-pvc; do
        [ -d "$d" ] || continue
        base=${d##*/}
        id=${base##*_ws-}
        id=${id%-pvc}
        printf '  %-18s %s\n' "ws-${id}" "$d"
        cnt=$((cnt + 1))
    done
    printf '\n共 %s 个\n' "$cnt"
}

main() {
    local i rc

    detect_root
    [ -z "$LOG_FILE" ] && LOG_FILE="$SCRIPT_DIR/logs/oss-migrate.log"

    if [ "$LIST_ONLY" -eq 1 ]; then
        preflight_root_only
        list_local
        return 0
    fi

    preflight

    [ -n "$MODE" ] || die "请指定迁移范围：--all 或 -id <ID 列表>（--help 查看用法）"

    # 演练模式不创建/写入日志文件（--skip-done 仍可读取已存在的日志）
    [ "$DRY_RUN" -eq 0 ] && log_init
    build_targets

    if [ "${#TARGET_IDS[@]}" -eq 0 ]; then
        warn "没有需要迁移的工作空间"
        printf '日志: %s\n' "$LOG_FILE"
        return 0
    fi

    print_plan
    confirm

    LOG_TMP=$(mktemp 2>/dev/null || printf '/tmp/oss-migrate.%s.$$' "$$")
    trap 'rm -f -- "$LOG_TMP"' EXIT INT TERM

    info "开始迁移，日志: ${LOG_FILE}"
    for ((i = 0; i < ${#TARGET_IDS[@]}; i++)); do
        migrate_one "${TARGET_IDS[$i]}" "${TARGET_DIRS[$i]}"
        rc=$?
        case "$rc" in
            0) CNT_OK=$((CNT_OK + 1)) ;;
            3) CNT_SKIP=$((CNT_SKIP + 1)) ;;
            *) CNT_FAIL=$((CNT_FAIL + 1)); FAILED_IDS[${#FAILED_IDS[@]}]="ws-${TARGET_IDS[$i]}" ;;
        esac
    done

    printf '\n%s===== 汇总 =====%s\n' "$C_INFO" "$C_END"
    printf '  成功 : %s\n  跳过 : %s\n  失败 : %s\n' "$CNT_OK" "$CNT_SKIP" "$CNT_FAIL"
    printf '  日志 : %s\n' "$LOG_FILE"

    if [ "${#FAILED_IDS[@]}" -gt 0 ]; then
        printf '  失败列表: %s\n' "$(csv "${FAILED_IDS[@]}")"
        printf '  重试命令: bash %s -id %s --yes\n' "$SCRIPT_NAME" \
            "$(csv "${FAILED_IDS[@]}" | sed 's/ws-//g')"
        return 1
    fi
    return 0
}

preflight_root_only() {
    resolve_root
}

main "$@"
exit $?
