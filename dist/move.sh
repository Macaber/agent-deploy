#!/usr/bin/env bash
set -Eeuo pipefail

SOURCE_ROOT="${SOURCE_ROOT:-/opt/bocom-defaults}"
BOCOMWORK_VERSION="${BOCOMWORK_VERSION:-}"

# 需求 1：复制文件前校验 source 和 target 是否一致（使用 cmp 快速对比字节）
copy_file_if_different() {
  local source="$1"
  local target="$2"

  # 兼容查找 $SOURCE_ROOT/opencode 和 $SOURCE_ROOT/opencodedir/opencode
  if [[ ! -f "$source" ]] && [[ -f "$(dirname "$source")/opencodedir/$(basename "$source")" ]]; then
    source="$(dirname "$source")/opencodedir/$(basename "$source")"
  fi

  if [[ ! -f "$source" ]]; then
    echo "skip：源文件不存在：$source" >&2
    return 0
  fi

  # 如果目标文件已存在，使用 cmp -s 对比文件内容是否完全一致
  if [[ -f "$target" ]] && cmp -s "$source" "$target"; then
    echo "skip：文件已存在且内容完全一致，跳过 copy：$target"
    return 0
  fi

  mkdir -p "$(dirname "$target")"
  echo "更新文件：$source -> $target"
  # 不用 cp -a：非 root 运行时无法保留源文件属主，busybox cp 可能因此报错，
  # 配合 set -e 会让整个启动链路失败。目标文件需已存在且属主为运行用户（见 Dockerfile 的 COPY --chown）。
  cp "$source" "$target"
}

# 需求 2 & 3：根据 BOCOMWORK_VERSION 环境变量校验并解压对应的 tar 包
# 支持传入第 3 个及后续参数作为“精准清理列表”
unpack_tar_if_version_matched() {
  local tar_prefix="$1"      # 压缩包前缀名（如 skills 或 dist）
  local target_dir="$2"      # 解压目标路径
  shift 2
  local clean_items=("$@")   # 需清理的指定子目录/文件列表（可选）

  # 1. 检查环境变量是否存在
  if [[ -z "$BOCOMWORK_VERSION" ]]; then
    echo "skip：未设置 BOCOMWORK_VERSION 环境变量，保留 Dockerfile 镜像内置的 $tar_prefix 资源"
    return 0
  fi

  local tar_file="$SOURCE_ROOT/${tar_prefix}-${BOCOMWORK_VERSION}.tar"

  # 兼容带 .gz 压缩后缀的情况（如 dist-260804.tar.gz）
  if [[ ! -f "$tar_file" ]] && [[ -f "${tar_file}.gz" ]]; then
    tar_file="${tar_file}.gz"
  fi

  # 2. 检查对应的 tar 文件是否存在于挂载源中
  if [[ ! -f "$tar_file" ]]; then
    echo "skip：未在 $SOURCE_ROOT 下找到匹配的压缩包：$(basename "$tar_file")，保留 Dockerfile 镜像内置的 $tar_prefix 资源"
    return 0
  fi

  mkdir -p "$target_dir"

  # 3. 精确清理指定的子项（避开 /app 下的其他重要文件）
  if [[ ${#clean_items[@]} -gt 0 ]]; then
    for item in "${clean_items[@]}"; do
      if [[ "$item" == "*" ]]; then
        echo "清理旧产物：$target_dir/*"
        rm -rf "${target_dir:?}"/*
      elif [[ -e "$target_dir/$item" ]]; then
        echo "清理旧产物目录：$target_dir/$item"
        rm -rf "$target_dir/$item"
      fi
    done
  fi

  echo "更新：检测到版本匹配，从 $tar_file 解压到 $target_dir"

  # 4. 解压覆盖
  tar -xf "$tar_file" -C "$target_dir"

  # 5. 清理可能误打包进来的垃圾文件
  rm -f "$target_dir"/*.tar "$target_dir"/*.tar.gz 2>/dev/null || true
}

echo "=== 开始 Bocomwork 容器启动初始化 ==="
echo "环境变量 BOCOMWORK_VERSION=${BOCOMWORK_VERSION:-<未设置>}"

# 1. 处理 opencode（目标已存在且文件一致则跳过 copy）
copy_file_if_different \
  "$SOURCE_ROOT/opencode" \
  "/usr/local/bin/opencode"

if [[ -f "/usr/local/bin/opencode" ]]; then
  chmod +x "/usr/local/bin/opencode"
fi


# 3. 动态替换 dist 目录：只精确删除 out 和 packages，绝对不影响 /app 下的其他文件
unpack_tar_if_version_matched \
  "dist" \
  "/app" \
  "out" \
  "packages" 

echo "=== Bocomwork 容器启动初始化完成 ==="
