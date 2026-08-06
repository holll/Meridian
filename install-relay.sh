#!/usr/bin/env bash
set -euo pipefail

# Meridian Relay 一键安装脚本
# 支持: install / update / uninstall

_detect_repo_from_git() {
    local remote
    remote=$(git -C "$(dirname -- "$0")" remote get-url origin 2>/dev/null || true)
    printf '%s\n' "$remote" | sed -n 's|.*github\.com[:/]\([^/][^/]*/[^/.][^.]*\).*|\1|p' | head -1
}
REPO="${MERIDIAN_REPO:-}"
if [ -z "$REPO" ]; then
    REPO=$(_detect_repo_from_git)
fi
[ -n "$REPO" ] || REPO="holll/Meridian"

INSTALL_DIR="${MERIDIAN_INSTALL_DIR:-/opt/meridian/relay}"
DATA_DIR="${MERIDIAN_DATA_DIR:-/opt/meridian/relay}"
SERVICE_FILE="${MERIDIAN_SERVICE_FILE:-/etc/systemd/system/meridian-relay.service}"
SERVICE_NAME="${MERIDIAN_SERVICE_NAME:-meridian-relay}"
BIN_NAME="meridian-relay"
ROOT_GROUP="${MERIDIAN_ROOT_GROUP:-$(id -gn 0 2>/dev/null || printf 'root')}"
ASSUME_YES="${MERIDIAN_ASSUME_YES:-0}"

# Relay node configuration — when all of MASTER_URL/RELAY_TOKEN/RELAY_NAME are
# provided, installation runs non-interactively (supports -y / automation).
MASTER_URL="${MASTER_URL:-}"
RELAY_TOKEN="${RELAY_TOKEN:-}"
RELAY_NAME="${RELAY_NAME:-}"
RELAY_ISP="${RELAY_ISP:-}"
RELAY_PORT="${RELAY_PORT:-9091}"

PREVIOUS_BIN="${INSTALL_DIR}/${BIN_NAME}.previous"
ROOT_PREFIX=()

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
NC='\033[0m'

info() { printf "${CYAN}[INFO]${NC} %s\n" "$*"; }
ok()   { printf "${GREEN}[OK]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
fail() { printf "${RED}[ERROR]${NC} %s\n" "$*" >&2; exit 1; }

need_cmd() { command -v "$1" >/dev/null 2>&1 || fail "缺少必要命令: $1"; }

init_privilege() {
    if [ "${EUID}" -eq 0 ]; then
        ROOT_PREFIX=()
        return
    fi
    need_cmd sudo
    sudo -v
    ROOT_PREFIX=(sudo)
}

as_root() { "${ROOT_PREFIX[@]}" "$@"; }

is_systemd() {
    [ "$(uname -s)" = "Linux" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1
}

service_is_active() {
    is_systemd && systemctl is-active --quiet "$SERVICE_NAME"
}

ask_yes_no() {
    local prompt="$1" default_yes="${2:-0}" answer
    [ "$ASSUME_YES" = "1" ] && return 0
    if [ "$default_yes" = "1" ]; then
        read -r -p "$(printf "${CYAN}%s [Y/n]:${NC} " "$prompt")" answer
        [[ "$answer" != "n" && "$answer" != "N" ]]
    else
        read -r -p "$(printf "${CYAN}%s [y/N]:${NC} " "$prompt")" answer
        [[ "$answer" = "y" || "$answer" = "Y" ]]
    fi
}

detect_platform() {
    local os arch
    os=$(uname -s | tr '[:upper:]' '[:lower:]')
    arch=$(uname -m)
    case "$os" in
        linux) os="linux" ;;
        darwin) os="darwin" ;;
        *) fail "不支持的操作系统: $os" ;;
    esac
    case "$arch" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) fail "不支持的架构: $arch" ;;
    esac
    printf '%s-%s\n' "$os" "$arch"
}

valid_version() {
    [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]
}

get_latest_version() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 --retry 3 \
        --connect-timeout 15 -fsSL "https://api.github.com/repos/${REPO}/releases/latest" 2>/dev/null \
        | grep '"tag_name"' | head -1 | sed 's/.*"tag_name": *"//;s/".*//'
}

resolve_latest_version() {
    local version
    info "获取最新 Release..." >&2
    version=$(get_latest_version) || true
    valid_version "$version" || fail "无法获取有效的最新 Release 版本: ${version:-<empty>}"
    printf '%s\n' "$version"
}

get_current_version() {
    if [ -x "${INSTALL_DIR}/${BIN_NAME}" ]; then
        "${INSTALL_DIR}/${BIN_NAME}" --version 2>/dev/null || printf '已安装（版本未知）\n'
    else
        printf '\n'
    fi
}

download() {
    curl --proto '=https' --proto-redir '=https' --tlsv1.2 \
        --retry 3 --retry-delay 2 --connect-timeout 15 -fsSL "$1" -o "$2"
}

sha256_file() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 "$1" | awk '{print $1}'
    else
        fail "缺少 sha256sum 或 shasum，无法校验下载文件"
    fi
}

generate_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 32
    else
        od -An -N32 -tx1 /dev/urandom | tr -d ' \n'
    fi
}

DOWNLOADED_BINARY=""
download_release_binary() {
    local version="$1" tmp_dir="$2" suffix asset binary_file checksum_file expected actual
    suffix=$(detect_platform)
    asset="${BIN_NAME}-${suffix}"
    binary_file="${tmp_dir}/${asset}"
    checksum_file="${tmp_dir}/SHA256SUMS"
    info "下载 Meridian Relay ${version} (${suffix})..."
    download "https://github.com/${REPO}/releases/download/${version}/${asset}" "$binary_file" \
        || fail "二进制下载失败，请检查网络和 Release"
    download "https://github.com/${REPO}/releases/download/${version}/SHA256SUMS" "$checksum_file" \
        || fail "SHA256SUMS 下载失败；已停止安装"
    expected=$(awk -v file="$asset" '$2 == file || $2 == "*" file { print $1; exit }' "$checksum_file")
    printf '%s' "$expected" | grep -Eq '^[[:xdigit:]]{64}$' \
        || fail "SHA256SUMS 中缺少 ${asset} 的有效校验值"
    actual=$(sha256_file "$binary_file")
    expected=$(printf '%s' "$expected" | tr '[:upper:]' '[:lower:]')
    actual=$(printf '%s' "$actual" | tr '[:upper:]' '[:lower:]')
    [ "$expected" = "$actual" ] || fail "下载文件 SHA-256 校验失败"
    chmod 0755 "$binary_file"
    DOWNLOADED_BINARY="$binary_file"
    ok "SHA-256 校验通过"
}

env_file_path() { printf '%s/.env\n' "$DATA_DIR"; }

prompt_relay_config() {
    local master_url relay_token relay_name relay_isp
    if [ -n "$MASTER_URL" ] && [ -n "$RELAY_TOKEN" ] && [ -n "$RELAY_NAME" ]; then
        master_url=$(printf '%s' "$MASTER_URL" | sed 's|/$||')
        relay_token=$RELAY_TOKEN
        relay_name=$RELAY_NAME
        relay_isp=$RELAY_ISP
        info "使用环境变量配置节点（MASTER_URL / RELAY_TOKEN / RELAY_NAME）"
    else
        [ "$ASSUME_YES" = "1" ] \
            && fail "非交互模式缺少配置，请提供 MASTER_URL、RELAY_TOKEN、RELAY_NAME 环境变量"
        printf '\n%s\n\n' "请配置 Relay 节点参数（首次安装必填）："
        read -r -p "Master 面板地址（如 https://panel.example.com）: " master_url
        master_url=$(printf '%s' "$master_url" | sed 's|/$||')
        [ -n "$master_url" ] || fail "MASTER_URL 不能为空"
        read -r -s -p "RELAY_TOKEN（与 Master 配置完全一致）: " relay_token
        printf '\n'
        [ -n "$relay_token" ] || fail "RELAY_TOKEN 不能为空"
        read -r -p "节点名称（全局唯一，如 Unicom-SH）: " relay_name
        [ -n "$relay_name" ] || fail "RELAY_NAME 不能为空"
        read -r -p "运营商标识（可选，如 telecom/unicom/mobile/hk/oversea，回车跳过）: " relay_isp
    fi

    local env_file env_tmp
    env_file=$(env_file_path)
    env_tmp="${DATA_DIR}/.env.new"
    as_root install -d -o root -g root -m 0755 "$DATA_DIR"
    {
        printf 'PORT=%s\n' "$RELAY_PORT"
        printf 'PANEL_BIND_ADDR=0.0.0.0\n'
        printf 'MASTER_URL=%s\n' "$master_url"
        printf 'RELAY_TOKEN=%s\n' "$relay_token"
        printf 'RELAY_NAME=%s\n' "$relay_name"
        if [ -n "$relay_isp" ]; then
            printf 'RELAY_ISP=%s\n' "$relay_isp"
        fi
    } > "$env_tmp"
    chmod 0600 "$env_tmp"
    as_root install -o root -g root -m 0600 "$env_tmp" "${env_file}.install"
    rm -f -- "$env_tmp"
    as_root mv -f "${env_file}.install" "$env_file"
    ok "已创建配置: $env_file"
}

write_systemd_service() {
    is_systemd || return 0
    local service_tmp
    service_tmp=$(mktemp)
    cat > "$service_tmp" <<SVCEOF
[Unit]
Description=Meridian Relay distributed proxy node
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${DATA_DIR}/.env
ExecStart=${INSTALL_DIR}/${BIN_NAME}
WorkingDirectory=${DATA_DIR}
Restart=on-failure
RestartSec=5
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
RestrictRealtime=true
RestrictNamespaces=true
CapabilityBoundingSet=
MemoryDenyWriteExecute=true
SystemCallArchitectures=native
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SVCEOF
    as_root install -o root -g root -m 0644 "$service_tmp" "$SERVICE_FILE"
    rm -f -- "$service_tmp"
    as_root systemctl daemon-reload
    as_root systemctl enable "$SERVICE_NAME" >/dev/null
}

health_url() {
    local port
    port=$(grep -E '^PORT=' "$(env_file_path)" 2>/dev/null | head -1 | cut -d= -f2 || printf '9091')
    [ -n "$port" ] || port="9091"
    printf 'http://localhost:%s/healthz\n' "$port"
}

wait_for_health() {
    local attempts="${1:-15}" url i code
    command -v curl >/dev/null 2>&1 || return 0
    url=$(health_url)
    for ((i = 1; i <= attempts; i++)); do
        code=$(curl --noproxy '*' --proto '=http' --connect-timeout 1 --max-time 2 \
            -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
        [ "$code" = "200" ] && return 0
        sleep 1
    done
    # 进程存在且与 Master 心跳正常时 /healthz 返回 200；失联时降级 503
    is_systemd && systemctl is-active --quiet "$SERVICE_NAME" && return 0
    return 1
}

do_install() {
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" version tmp_dir
    need_cmd curl
    need_cmd awk
    need_cmd grep
    need_cmd install
    need_cmd mktemp
    need_cmd sed
    need_cmd tr
    init_privilege
    if [ -x "$current_binary" ]; then
        info "检测到已安装的 Meridian Relay $(get_current_version)；install 不会更新，请使用 update"
        return 0
    fi
    version=$(resolve_latest_version)
    tmp_dir=$(mktemp -d)
    chmod 0700 "$tmp_dir"
    download_release_binary "$version" "$tmp_dir"
    as_root install -d -o root -g "$ROOT_GROUP" -m 0755 "$INSTALL_DIR"
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$DOWNLOADED_BINARY" "${current_binary}.new"
    as_root mv -f "${current_binary}.new" "$current_binary"
    rm -rf -- "$tmp_dir"
    if ! as_root test -f "$(env_file_path)"; then
        prompt_relay_config
    fi
    write_systemd_service
    if is_systemd; then
        as_root systemctl restart "$SERVICE_NAME"
        sleep 3
        if service_is_active; then
            ok "Meridian Relay 服务已启动"
        else
            warn "服务可能未正常启动，请运行: journalctl -u ${SERVICE_NAME} -n 30"
        fi
    else
        warn "未检测到 systemd；已安装二进制，请手动加载 ${DATA_DIR}/.env 后启动"
    fi
    printf '\nMeridian Relay %s 安装完成\n' "$version"
    printf '  数据目录: %s\n' "$DATA_DIR"
    printf '  配置文件: %s/.env\n' "$DATA_DIR"
}

do_update() {
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" current_version latest_version tmp_dir
    [ -x "$current_binary" ] || fail "Meridian Relay 尚未安装，请先运行 install"
    need_cmd curl
    need_cmd mktemp
    init_privilege
    current_version=$(get_current_version)
    latest_version=$(resolve_latest_version)
    if [ "$current_version" = "$latest_version" ]; then
        ok "当前已是最新版本: $latest_version"
        return 0
    fi
    tmp_dir=$(mktemp -d)
    chmod 0700 "$tmp_dir"
    download_release_binary "$latest_version" "$tmp_dir"
    if is_systemd && service_is_active; then
        as_root systemctl stop "$SERVICE_NAME"
    fi
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$current_binary" "${PREVIOUS_BIN}.new"
    as_root mv -f "${PREVIOUS_BIN}.new" "$PREVIOUS_BIN"
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$DOWNLOADED_BINARY" "${current_binary}.new"
    as_root mv -f "${current_binary}.new" "$current_binary"
    rm -rf -- "$tmp_dir"
    if is_systemd; then
        as_root systemctl restart "$SERVICE_NAME"
        sleep 3
        if service_is_active; then
            ok "已更新到 $latest_version 并重启服务"
        else
            warn "服务重启后可能未正常运行，请检查: journalctl -u ${SERVICE_NAME} -n 30"
        fi
    else
        ok "已更新到 $latest_version（二进制已替换，请手动重启服务）"
    fi
}

do_uninstall() {
    init_privilege
    warn "即将卸载 Meridian Relay"
    [ "$ASSUME_YES" = "1" ] || ask_yes_no "确认卸载？" 0 || { info "已取消"; return 0; }
    if is_systemd && [ -f "$SERVICE_FILE" ]; then
        as_root systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        as_root systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        as_root rm -f -- "$SERVICE_FILE"
        as_root systemctl daemon-reload
    fi
    as_root rm -f -- "${INSTALL_DIR}/${BIN_NAME}" "$PREVIOUS_BIN" \
        "${INSTALL_DIR}/${BIN_NAME}.new"
    local remove_data=0
    if [ "$ASSUME_YES" != "1" ]; then
        ask_yes_no "是否同时删除数据目录 ${DATA_DIR}（包含配置）？" 0 && remove_data=1
    fi
    if [ "$remove_data" = "1" ] && [ -d "$DATA_DIR" ]; then
        as_root rm -rf -- "$DATA_DIR"
        ok "数据目录已删除"
    else
        info "数据目录已保留: $DATA_DIR"
    fi
    ok "Meridian Relay 已卸载"
}

usage() {
    cat <<'USAGE'
Meridian Relay 一键安装工具

用法:
  install-relay.sh install    首次安装最新版本（交互输入节点参数）
  install-relay.sh update     更新到最新 Release
  install-relay.sh uninstall  卸载节点程序
  install-relay.sh help       显示本帮助

节点配置（环境变量，齐备时跳过交互输入）:
  MASTER_URL              Master 面板地址，如 https://panel.example.com
  RELAY_TOKEN             与 Master 配置完全一致的共享密钥
  RELAY_NAME              节点名称（全局唯一）
  RELAY_ISP               运营商标识（可选: telecom/unicom/mobile/hk/oversea）
  RELAY_PORT              节点监听端口（默认 9091）

安装参数:
  MERIDIAN_REPO           GitHub 仓库（默认从 git remote 自动检测或使用 holll/Meridian）
  MERIDIAN_INSTALL_DIR    二进制安装目录（默认 /opt/meridian/relay）
  MERIDIAN_DATA_DIR       数据/配置目录（默认 /opt/meridian/relay）
  MERIDIAN_SERVICE_FILE   systemd 服务文件路径（默认 /etc/systemd/system/meridian-relay.service）
  MERIDIAN_ASSUME_YES=1   非交互模式（需配合 MASTER_URL/RELAY_TOKEN/RELAY_NAME 使用）

示例（非交互一行安装）:
  env MASTER_URL=https://panel.example.com RELAY_TOKEN=xxx RELAY_NAME=my-node \
      ./install-relay.sh install
USAGE
}

main_menu() {
    local current choice
    current=$(get_current_version)
    printf '\nMeridian Relay 安装工具\n'
    printf '  当前版本: %s\n\n' "${current:-未安装}"
    printf '  1) 安装\n'
    printf '  2) 更新到最新版\n'
    printf '  3) 卸载\n'
    printf '  0) 退出\n\n'
    read -r -p "请选择 [0-3]: " choice
    case "$choice" in
        1) do_install ;;
        2) do_update ;;
        3) do_uninstall ;;
        0) exit 0 ;;
        *) fail "无效选项" ;;
    esac
}

run_cli() {
    local action="${1:-menu}"
    [ "$#" -eq 0 ] || shift
    while [ "$#" -gt 0 ]; do
        case "$1" in
            -y|--yes) ASSUME_YES=1 ;;
            -h|--help) action="help" ;;
            *) fail "未知参数: $1" ;;
        esac
        shift
    done
    case "$action" in
        install)   do_install ;;
        update)    do_update ;;
        uninstall) do_uninstall ;;
        help)      usage ;;
        menu)      main_menu ;;
        *) fail "未知操作: $action（仅支持 install、update、uninstall、help）" ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    run_cli "$@"
fi
