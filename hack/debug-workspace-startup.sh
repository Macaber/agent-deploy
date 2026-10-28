#!/usr/bin/env bash
#
# debug-workspace-startup.sh —— 采集 Workspace「停止 → 唤醒」全链路启动时序
#
# 用法：
#   终端 1： ./hack/debug-workspace-startup.sh watch      # 常驻，采集停止/唤醒全过程
#   终端 2： 另外去 stop 再 wake（kubectl 或控制台都行）
#   终端 1： Ctrl-C 结束 watch
#   终端 2： ./hack/debug-workspace-startup.sh collect    # 打完收工，把目录发我
#
# 环境变量：
#   NS=bocomwork         Workspace 所在命名空间
#   WS=ws-9526043        Workspace 名称（也是 Pod 名前缀）
#   FUSE_NS=ack-csi-fuse OSS FUSE Pod 所在命名空间
#   OUT_ROOT=/tmp/ws-startup-debug  输出根目录（默认不污染 git 仓库）
#
set -uo pipefail

NS="${NS:-bocomwork}"
WS="${WS:-ws-9526043}"
FUSE_NS="${FUSE_NS:-ack-csi-fuse}"
OUT_ROOT="${OUT_ROOT:-/tmp/ws-startup-debug}"
PV="${PV:-${WS}-pv}"

C_RESET=$'\033[0m'; C_BOLD=$'\033[1m'; C_DIM=$'\033[2m'
say()  { printf '%s\n' "$*"; }
head_() { printf '\n%s%s%s\n' "$C_BOLD" "$*" "$C_RESET"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
now()  { date +%H:%M:%S; }

need() { command -v "$1" >/dev/null 2>&1 || die "缺少命令：$1"; }

# 最新（创建时间最晚）的 Pod —— 唤醒后 Pod 名会变，必须按 label 找而不是写死名字
newest_pod() {
  kubectl -n "$NS" get pod -l "app=workspace,workspace=$WS" \
    -o jsonpath='{range .items[*]}{.metadata.creationTimestamp}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null \
    | sort | tail -n1 | awk 'NF{print $2}'
}

# 把 Pod 的三个关键时间点抠出来：创建 / 容器启动 / Ready
pod_timeline() {
  local pod="$1"
  kubectl -n "$NS" get pod "$pod" -o jsonpath='{.metadata.creationTimestamp}{"\n"}{.status.containerStatuses[0].state.running.startedAt}{"\n"}{.status.conditions[?(@.type=="Ready")].lastTransitionTime}{"\n"}{.status.containerStatuses[0].restartCount}{"\n"}{.status.containerStatuses[0].lastState.terminated.reason}{"\n"}' 2>/dev/null
}

usage() {
  awk 'NR>2 {if ($0 ~ /^#/) {sub(/^# ?/, ""); print; next} exit}' "$0"
  exit "${1:-0}"
}

# ---------------------------------------------------------------------------
# watch：常驻采集，只有状态变化时才写日志，避免刷屏
# ---------------------------------------------------------------------------
cmd_watch() {
  need kubectl
  local dir="$OUT_ROOT/watch-$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$dir"
  printf '%s\n' "$dir" > "$OUT_ROOT/LATEST"

  head_ "采集目录：$dir"
  say "现在去【停止】再【唤醒】。启动过程中不要关闭本窗口，看到 Pod Ready 后按 Ctrl-C。"
  say "提示：唤醒后 Pod 名会变，本脚本已按 label 自动发现，无需你改任何东西。"

  # 1. 业务命名空间事件流（含 FailedMount / Unhealthy / Killing 等 Warning）
  (
    kubectl -n "$NS" get events -w --sort-by=.lastTimestamp \
      -o custom-columns=LAST:.lastTimestamp,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message \
      --no-headers 2>&1 | while IFS= read -r l; do printf '%s | %s\n' "$(now)" "$l"; done
  ) > "$dir/events-workspace.log" 2>&1 &

  # 2. FUSE Pod 命名空间事件流（挂载失败/重启的证据）
  (
    kubectl -n "$FUSE_NS" get events -w --sort-by=.lastTimestamp \
      -o custom-columns=LAST:.lastTimestamp,TYPE:.type,REASON:.reason,OBJECT:.involvedObject.name,MESSAGE:.message \
      --no-headers 2>&1 | while IFS= read -r l; do printf '%s | %s\n' "$(now)" "$l"; done
  ) > "$dir/events-fuse.log" 2>&1 &

  # 3. 业务 Pod 状态轮询（0.5s，仅变化时输出）
  (
    last=""
    while :; do
      cur="$(kubectl -n "$NS" get pod -l "app=workspace,workspace=$WS" \
        -o jsonpath='{range .items[*]}{.metadata.name} phase={.status.phase} ready={.status.containerStatuses[0].ready} restarts={.status.containerStatuses[0].restartCount}{"\n"}{end}' 2>/dev/null)"
      if [ "$cur" != "$last" ]; then
        printf '%s | %s\n' "$(now)" "$(printf '%s' "$cur" | tr '\n' ';')"
        last="$cur"
      fi
      sleep 0.5
    done
  ) > "$dir/pod-state.log" 2>&1 &

  # 4. FUSE Pod 状态轮询（1s，仅变化时输出）
  (
    last=""
    while :; do
      cur="$(kubectl -n "$FUSE_NS" get pod \
        -o jsonpath='{range .items[*]}{.metadata.name} created={.metadata.creationTimestamp} phase={.status.phase} started={.status.containerStatuses[0].state.running.startedAt} ready={.status.containerStatuses[0].ready}{"\n"}{end}' 2>/dev/null)"
      if [ "$cur" != "$last" ]; then
        printf '%s |\n%s\n' "$(now)" "$cur"
        last="$cur"
      fi
      sleep 1
    done
  ) > "$dir/fuse-state.log" 2>&1 &

  # 5. Workspace phase 轮询（1s，仅变化时输出）
  (
    last=""
    while :; do
      cur="$(kubectl -n "$NS" get workspace "$WS" -o jsonpath='{.status.phase} pod={.status.podName} endpoint={.status.Endpoint}' 2>/dev/null)"
      if [ "$cur" != "$last" ]; then
        printf '%s | %s\n' "$(now)" "$cur"
        last="$cur"
      fi
      sleep 1
    done
  ) > "$dir/workspace-phase.log" 2>&1 &

  cleanup() {
    trap - INT TERM EXIT
    local p; p="$(jobs -p)"
    if [ -n "$p" ]; then
      kill $p 2>/dev/null
      disown -a 2>/dev/null
    fi
    say ""
    say "已停止采集。目录： $dir"
    say "接着执行： ./hack/debug-workspace-startup.sh collect"
    exit 0
  }
  trap cleanup INT TERM EXIT
  wait
}

# ---------------------------------------------------------------------------
# collect：唤醒完成后抓全部证据（事件会过期，这里落盘的是不会过期的快照）
# ---------------------------------------------------------------------------
cmd_collect() {
  need kubectl
  local dir="$OUT_ROOT/collect-$(date +%Y%m%d-%H%M%S)"
  mkdir -p "$dir"

  local pod; pod="$(newest_pod)"
  [ -n "$pod" ] || die "没找到 label 为 workspace=$WS 的 Pod，确认 NS=$NS / WS=$WS 是否正确"

  head_ "Pod: $pod"
  say "抓取中……（约 10~30 秒）"

  # ---------- 核心：三个时间点 ----------
  local created started ready restarts term
  { read -r created; read -r started; read -r ready; read -r restarts; read -r term; } <<<"$(pod_timeline "$pod")"

  {
    echo "pod=$pod"
    echo "created=$created"
    echo "started=$started"
    echo "ready=$ready"
    echo "restarts=$restarts"
    echo "last_terminated_reason=$term"
  } > "$dir/timeline.txt"

  # ---------- Pod / 控制器 / 存储对象 ----------
  kubectl -n "$NS" get pod "$pod" -o yaml            > "$dir/pod.yaml" 2>&1 || true
  kubectl -n "$NS" describe pod "$pod"               > "$dir/pod.describe" 2>&1 || true
  kubectl -n "$NS" get workspace "$WS" -o yaml       > "$dir/workspace.yaml" 2>&1 || true
  kubectl -n "$NS" get deploy,rs -l "workspace=$WS" -o wide > "$dir/deploy-rs.txt" 2>&1 || true
  kubectl -n "$NS" get pvc,pv -o wide                > "$dir/pvc-pv.txt" 2>&1 || true
  kubectl get pv "$PV" -o yaml                       > "$dir/pv.yaml" 2>&1 || true
  kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$dir/events-snapshot.txt" 2>&1 || true
  kubectl -n "$FUSE_NS" get events --sort-by=.lastTimestamp > "$dir/events-fuse-snapshot.txt" 2>&1 || true

  # ---------- FUSE Pod（这次启动相关的那个） ----------
  local fuse; fuse="$(kubectl -n "$FUSE_NS" get pod \
      -o jsonpath='{range .items[*]}{.metadata.creationTimestamp}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null \
      | sort | grep -- "$WS" | tail -n1 | awk 'NF{print $2}')"
  if [ -n "$fuse" ]; then
    kubectl -n "$FUSE_NS" get pod "$fuse" -o yaml      > "$dir/fuse-pod.yaml" 2>&1 || true
    kubectl -n "$FUSE_NS" describe pod "$fuse"         > "$dir/fuse-pod.describe" 2>&1 || true
    kubectl -n "$FUSE_NS" logs "$fuse" --all-containers=true --prefix=true \
      --timestamps --tail=200 > "$dir/fuse-pod.log" 2>&1 || true
  else
    echo "未找到名字含 $WS 的 FUSE Pod（可能已被回收）" > "$dir/fuse-pod.missing"
  fi

  # ---------- 应用日志（含上一次被 kill 的） ----------
  kubectl -n "$NS" logs "$pod" --all-containers=true --prefix=true \
    --timestamps --tail=400 > "$dir/app.log" 2>&1 || true
  kubectl -n "$NS" logs "$pod" --all-containers=true --prefix=true \
    --timestamps --previous --tail=400 > "$dir/app.previous.log" 2>&1 || true

  # ---------- CSI 节点插件：NodePublish / 等 FUSE Pod 的过程 ----------
  local node
  node="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null | head -n1 | tr -d '[:space:]')"
  echo "node=$node" > "$dir/node.txt"
  if [ -n "$node" ]; then
    kubectl -n kube-system get pod -o wide --field-selector "spec.nodeName=$node" \
      > "$dir/kube-system-pods-on-node.txt" 2>&1 || true
    local csi
    # ACK 节点侧插件通常叫 csi-plugin-xxxx；优先它，其次任何名字含 csi 的 Pod
    csi="$(awk '/^csi-plugin-/ {print $1}' "$dir/kube-system-pods-on-node.txt" | head -n1)"
    [ -n "$csi" ] || csi="$(awk '/csi/ && !/NAME/ {print $1}' "$dir/kube-system-pods-on-node.txt" | head -n1)"
    if [ -n "$csi" ]; then
      echo "csi_pod=$csi" >> "$dir/node.txt"
      # 该 Pod 是多容器（csi-plugin / csi-ossfs ...），必须 --all-containers，否则报错
      kubectl -n kube-system logs "$csi" --all-containers=true --prefix=true \
        --timestamps --tail=800 > "$dir/csi-plugin.log" 2>&1 || true
    fi
  fi

  # ---------- /health 实测耗时（尽力而为，容器没有 curl 就跳过） ----------
  local port
  port="$(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[0].ports[0].containerPort}' 2>/dev/null | head -n1 | tr -d '[:space:]')"
  port="${port:-3001}"
  {
    echo "# 连续 5 次 curl /health，间隔 2s，端口 $port"
    kubectl -n "$NS" exec "$pod" -c runtime -- sh -c "
      for i in 1 2 3 4 5; do
        if command -v curl >/dev/null 2>&1; then
          curl -sS -m 10 -o /dev/null -w \"try\$i code=%{http_code} connect=%{time_connect}s total=%{time_total}s\n\" http://127.0.0.1:$port/health
        else
          echo 'try' \$i 'curl not found in container'
        fi
        sleep 2
      done" 2>&1
  } > "$dir/health-probe.txt" 2>&1 || true

  # ---------- 汇总 ----------
  local report; report="$dir/SUMMARY.txt"
  {
    echo "Workspace : $WS   (namespace=$NS)"
    echo "Pod       : $pod"
    echo "Node      : ${node:-unknown}"
    echo "FUSE pod  : ${fuse:-未找到}"
    echo
    echo "created = $created"
    echo "started = $started"
    echo "ready   = $ready"
    echo "restarts= $restarts"
    echo "lastTerminatedReason = $term"
    echo
  } > "$report"

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$created" "$started" "$ready" "$restarts" "$term" >> "$report" 2>/dev/null <<'PY' || true
import sys, datetime
def p(s):
    s = s.strip()
    if not s: return None
    return datetime.datetime.strptime(s.replace("Z", "+0000"), "%Y-%m-%dT%H:%M:%S%z")
created, started, ready = (p(sys.argv[1]), p(sys.argv[2]), p(sys.argv[3]))
def d(a, b):
    if a and b: return "%.1fs" % (b - a).total_seconds()
    return "n/a"
print("T1 创建 -> T2 容器启动（调度+挂载+sandbox，K8s 侧） :", d(created, started))
print("T2 启动 -> T3 就绪（应用初始化 + 探针通过）        :", d(started, ready))
print("T1 创建 -> T3 就绪（用户感知的总启动时长）         :", d(created, ready))
if (sys.argv[4] or "").strip() not in ("", "0"):
    print("!! restartCount =", sys.argv[4], "—— 说明被探针杀过，总时长是多次累加")
if (sys.argv[5] or "").strip():
    print("!! 上一次容器终止原因 =", sys.argv[5])
PY
  fi

  tar czf "$dir.tgz" -C "$(dirname "$dir")" "$(basename "$dir")" 2>/dev/null && \
    say "打包：$dir.tgz"

  head_ "===== 摘要 ====="
  cat "$report"
  head_ "================="
  say ""
  say "请把 ${dir}.tgz 发我（或直接把 SUMMARY.txt + app.log + events-*.log 贴过来）。"
  say "目录里每个文件的用途："
  say "  SUMMARY.txt        三段耗时 + 重启判定（最重要，先看这个）"
  say "  app.log            应用自己的启动日志（带时间戳，看 started->ready 卡在哪一步）"
  say "  events-*.log       watch 期间实时抓到的事件，含 FailedMount / Unhealthy / Killing"
  say "  csi-plugin.log     节点侧 NodePublish 日志，能确认是否在等 FUSE Pod"
  say "  health-probe.txt   /health 实测耗时，判断是否 pegged 在 ossfs I/O 上"
  say "  pod.yaml / pv.yaml 实际生效的探针、resources、ossfs 挂载参数（otherOpts）"
}

case "${1:-}" in
  watch)   cmd_watch ;;
  collect) cmd_collect ;;
  -h|--help|help) usage 0 ;;
  "")      usage 1 ;;
  *)       die "未知子命令：$1（可选 watch / collect）" ;;
esac
