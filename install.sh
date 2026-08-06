#!/usr/bin/env bash
set -euo pipefail

# Meridian one-click installer.
# Public operations are intentionally limited to install, update, password,
# and uninstall. Backups and rollback remain internal safety mechanisms.

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
INSTALL_DIR="${MERIDIAN_INSTALL_DIR:-/usr/local/bin}"
DATA_DIR="${MERIDIAN_DATA_DIR:-/opt/meridian}"
BACKUP_DIR="${MERIDIAN_BACKUP_DIR:-/opt/meridian-backups}"
SERVICE_FILE="${MERIDIAN_SERVICE_FILE:-/etc/systemd/system/meridian.service}"
SERVICE_NAME="${MERIDIAN_SERVICE_NAME:-meridian}"
BIN_NAME="meridian"
ROOT_GROUP="${MERIDIAN_ROOT_GROUP:-$(id -gn 0 2>/dev/null || printf 'root')}"

while [ "$INSTALL_DIR" != "/" ] && [[ "$INSTALL_DIR" == */ ]]; do INSTALL_DIR="${INSTALL_DIR%/}"; done
while [ "$DATA_DIR" != "/" ] && [[ "$DATA_DIR" == */ ]]; do DATA_DIR="${DATA_DIR%/}"; done
while [ "$BACKUP_DIR" != "/" ] && [[ "$BACKUP_DIR" == */ ]]; do BACKUP_DIR="${BACKUP_DIR%/}"; done

PREVIOUS_BIN="${INSTALL_DIR}/${BIN_NAME}.previous"
ASSUME_YES="${MERIDIAN_ASSUME_YES:-0}"
PURGE_DATA=0
INITIAL_SETUP_TOKEN=""
LAST_BACKUP_PATH=""
ROOT_PREFIX=()
UPDATE_TMP_DIR=""
UPDATE_WAS_ACTIVE=0
UPDATE_BINARY_CHANGED=0
UPDATE_TRANSACTION=0
PASSWORD_TMP_DIR=""
PASSWORD_SNAPSHOT_DIR=""
PASSWORD_DB_PATH=""
PASSWORD_TRANSACTION=0
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

info() { printf "${CYAN}[INFO]${NC} %s\n" "$*"; }
ok() { printf "${GREEN}[OK]${NC} %s\n" "$*"; }
warn() { printf "${YELLOW}[WARN]${NC} %s\n" "$*"; }
fail() { printf "${RED}[ERROR]${NC} %s\n" "$*" >&2; exit 1; }

need_cmd() {
    command -v "$1" >/dev/null 2>&1 || fail "缺少必要命令: $1"
}

init_privilege() {
    if [ "${EUID}" -eq 0 ]; then
        ROOT_PREFIX=()
        return
    fi
    need_cmd sudo
    sudo -v
    ROOT_PREFIX=(sudo)
}

as_root() {
    "${ROOT_PREFIX[@]}" "$@"
}

is_systemd() {
    [ "$(uname -s)" = "Linux" ] && [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1
}

service_is_active() {
    is_systemd && systemctl is-active --quiet "$SERVICE_NAME"
}

ask_yes_no() {
    local prompt="$1" default_yes="${2:-0}" answer
    if [ "$ASSUME_YES" = "1" ]; then
        return 0
    fi
    if [ "$default_yes" = "1" ]; then
        read -r -p "$(printf "${CYAN}%s [Y/n]:${NC} " "$prompt")" answer
        [[ "$answer" != "n" && "$answer" != "N" ]]
    else
        read -r -p "$(printf "${CYAN}%s [y/N]:${NC} " "$prompt")" answer
        [[ "$answer" = "y" || "$answer" = "Y" ]]
    fi
}

validate_safe_directory() {
    local value="$1" label="$2"
    case "$value" in
        ""|/|/bin|/boot|/dev|/etc|/home|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/var)
            fail "拒绝使用不安全的${label}: ${value:-<empty>}"
            ;;
        *//*|*/../*|*/..|*/./*|*/.|*$'\n'*)
            fail "${label}包含不安全的路径片段: $value"
            ;;
    esac
    [[ "$value" = /* ]] || fail "${label}必须是绝对路径: $value"
}

validate_data_dir() {
    validate_safe_directory "$DATA_DIR" "数据目录"
}

validate_backup_dir() {
    validate_safe_directory "$BACKUP_DIR" "备份目录"
}

validate_db_path() {
    local db_path="$1"
    case "$db_path" in
        ""|/|/bin|/boot|/dev|/etc|/home|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/var|*//*|*/../*|*/..|*/./*|*/.|*$'\n'*)
            return 1
            ;;
    esac
    [[ "$db_path" = /* ]]
}

valid_version() {
    [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z.-]+)?$ ]]
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

download_release_binary() {
    local version="$1" tmp_dir="$2" suffix asset binary_file checksum_file expected actual
    suffix=$(detect_platform)
    asset="${BIN_NAME}-${suffix}"
    binary_file="${tmp_dir}/${asset}"
    checksum_file="${tmp_dir}/SHA256SUMS"
    info "下载 Meridian ${version} (${suffix})..."
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

env_file_path() {
    printf '%s/.env\n' "$DATA_DIR"
}

read_env_value() {
    local key="$1" env_file value=""
    env_file=$(env_file_path)
    if [ -f "$env_file" ] || as_root test -f "$env_file" 2>/dev/null; then
        # $1 is an awk field reference, not a shell variable.
        # shellcheck disable=SC2016
        value=$(as_root awk -F= -v wanted="$key" '$1 == wanted { sub(/^[^=]*=/, ""); print; exit }' "$env_file" 2>/dev/null || true)
    fi
    printf '%s\n' "$value"
}

env_has_key() {
    local key="$1" env_file
    env_file=$(env_file_path)
    # $1 is an awk field reference, not a shell variable.
    # shellcheck disable=SC2016
    as_root awk -F= -v wanted="$key" '$1 == wanted { found=1; exit } END { exit !found }' "$env_file" 2>/dev/null
}

install_env_file() {
    local source_file="$1" env_file
    env_file=$(env_file_path)
    as_root install -o root -g "$ROOT_GROUP" -m 0600 "$source_file" "${env_file}.new"
    as_root mv -f "${env_file}.new" "$env_file"
}

write_rotated_env() {
    local secret="$1" output="$2" env_file
    env_file=$(env_file_path)
    # $1 is an awk field reference, not a shell variable.
    # shellcheck disable=SC2016
    as_root awk -F= '$1 != "JWT_SECRET" { print }' "$env_file" > "$output"
    printf 'JWT_SECRET=%s\n' "$secret" >> "$output"
    chmod 0600 "$output"
}

read_config_port() {
    local configured
    configured=$(read_env_value PORT)
    if [[ "$configured" =~ ^[0-9]+$ ]] && [ "$configured" -ge 1 ] && [ "$configured" -le 65535 ]; then
        printf '%s\n' "$configured"
    else
        printf '9090\n'
    fi
}

health_url() {
    printf 'http://localhost:%s/api/auth/check\n' "$(read_config_port)"
}

wait_for_health() {
    local attempts="${1:-20}" url code i
    command -v curl >/dev/null 2>&1 || return 1
    url=$(health_url)
    for ((i = 1; i <= attempts; i++)); do
        code=$(curl --noproxy '*' --proto '=http' --connect-timeout 1 --max-time 2 \
            -sS -o /dev/null -w '%{http_code}' "$url" 2>/dev/null || true)
        [ "$code" = "200" ] && return 0
        sleep 1
    done
    return 1
}

prepare_data_and_config() {
    local tmp_dir="$1" env_file secret env_tmp
    validate_data_dir
    env_file=$(env_file_path)
    as_root install -d -o root -g "$ROOT_GROUP" -m 0755 "$DATA_DIR"

    if ! as_root test -f "$env_file"; then
        secret=$(generate_secret)
        INITIAL_SETUP_TOKEN=$(generate_secret)
        env_tmp="${tmp_dir}/meridian.env"
        printf 'JWT_SECRET=%s\nSETUP_TOKEN=%s\nPORT=9090\nDB_PATH=%s/meridian.db\nPANEL_BIND_ADDR=0.0.0.0\n' \
            "$secret" "$INITIAL_SETUP_TOKEN" "$DATA_DIR" > "$env_tmp"
        chmod 0600 "$env_tmp"
        install_env_file "$env_tmp"
        ok "已创建安全配置: $env_file"
    else
        as_root test -L "$env_file" && fail "拒绝修改符号链接形式的配置文件: $env_file"
        info "保留现有配置: $env_file"
    fi
}

write_systemd_service() {
    local tmp_dir="$1" service_tmp
    is_systemd || return 0
    service_tmp="${tmp_dir}/meridian.service"
    cat > "$service_tmp" <<SVCEOF
[Unit]
Description=Meridian Emby reverse proxy management panel
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
EnvironmentFile=${DATA_DIR}/.env
ExecStart=${INSTALL_DIR}/${BIN_NAME}
WorkingDirectory=${DATA_DIR}
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SVCEOF
    as_root install -o root -g root -m 0644 "$service_tmp" "$SERVICE_FILE"
    as_root systemctl daemon-reload
    as_root systemctl enable "$SERVICE_NAME" >/dev/null
}

ensure_no_manual_process() {
    local binary="${INSTALL_DIR}/${BIN_NAME}"
    if command -v pgrep >/dev/null 2>&1 && pgrep -f -- "$binary" >/dev/null 2>&1; then
        fail "检测到手动运行的 Meridian；请先停止进程再更新，以保证数据库备份一致"
    fi
}

create_backup_archive() {
    local label="$1" stamp safe_label archive archive_tmp data_parent data_base
    validate_data_dir
    validate_backup_dir
    as_root test -d "$DATA_DIR" || return 1
    need_cmd tar
    safe_label=$(printf '%s' "$label" | tr -cd 'A-Za-z0-9._-')
    [ -n "$safe_label" ] || safe_label="internal"
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    archive="${BACKUP_DIR}/${BIN_NAME}-${safe_label}-${stamp}-$$.tar.gz"
    archive_tmp="${archive}.tmp.$$"
    data_parent=$(dirname -- "$DATA_DIR")
    data_base=$(basename -- "$DATA_DIR")
    as_root install -d -o root -g "$ROOT_GROUP" -m 0700 "$BACKUP_DIR"
    if ! as_root tar -C "$data_parent" -czf "$archive_tmp" "$data_base"; then
        as_root rm -f -- "$archive_tmp"
        return 1
    fi
    as_root chmod 0600 "$archive_tmp"
    as_root mv -f "$archive_tmp" "$archive"
    LAST_BACKUP_PATH="$archive"
}

restore_previous_binary() {
    [ -f "$PREVIOUS_BIN" ] || return 1
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$PREVIOUS_BIN" "${INSTALL_DIR}/${BIN_NAME}.rollback"
    as_root mv -f "${INSTALL_DIR}/${BIN_NAME}.rollback" "${INSTALL_DIR}/${BIN_NAME}"
}

cleanup_update_transaction() {
    local exit_code=$?
    if [ "$exit_code" -ne 0 ] && [ "$UPDATE_TRANSACTION" = "1" ]; then
        warn "更新中断，正在恢复更新前的二进制和服务状态..."
        if [ "$UPDATE_BINARY_CHANGED" = "1" ]; then
            restore_previous_binary || true
        fi
        if is_systemd; then
            if [ "$UPDATE_WAS_ACTIVE" = "1" ]; then
                as_root systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 || true
            else
                as_root systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
            fi
        fi
        UPDATE_TRANSACTION=0
        UPDATE_BINARY_CHANGED=0
    fi
    if [ -n "$UPDATE_TMP_DIR" ] && [ -d "$UPDATE_TMP_DIR" ] && [ "$UPDATE_TMP_DIR" != "/" ]; then
        rm -rf -- "$UPDATE_TMP_DIR"
    fi
    return "$exit_code"
}

abort_update_transaction() {
    trap - INT TERM
    exit 130
}

do_install() {
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" tmp_dir version
    need_cmd curl
    need_cmd awk
    need_cmd grep
    need_cmd install
    need_cmd mktemp
    need_cmd sed
    need_cmd tr
    init_privilege
    validate_data_dir
    validate_backup_dir

    if [ -x "$current_binary" ]; then
        info "检测到已安装的 Meridian $(get_current_version)；install 不会执行更新"
        return 0
    fi

    version=$(resolve_latest_version)
    tmp_dir=$(mktemp -d)
    chmod 0700 "$tmp_dir"
    download_release_binary "$version" "$tmp_dir"
    prepare_data_and_config "$tmp_dir"
    write_systemd_service "$tmp_dir"
    as_root install -d -o root -g "$ROOT_GROUP" -m 0755 "$INSTALL_DIR"
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$DOWNLOADED_BINARY" "${current_binary}.new"
    as_root mv -f "${current_binary}.new" "$current_binary"

    if is_systemd; then
        if ! as_root systemctl restart "$SERVICE_NAME" || ! wait_for_health 20; then
            as_root rm -f -- "$current_binary"
            as_root systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
            rm -rf -- "$tmp_dir"
            fail "首次启动未通过健康检查；二进制已移除，数据与配置已保留"
        fi
        ok "Meridian 服务健康检查通过"
    else
        warn "未检测到 systemd；已安装二进制，但需要手动加载 ${DATA_DIR}/.env 后启动"
    fi
    rm -rf -- "$tmp_dir"

    printf '\n%s\n' "Meridian ${version} 安装完成"
    printf '  面板地址: http://服务器IP:%s\n' "$(read_config_port)"
    printf '  数据目录: %s\n' "$DATA_DIR"
    if [ -n "$INITIAL_SETUP_TOKEN" ]; then
        printf "  ${YELLOW}首次初始化令牌（请立即保存）:${NC} ${BOLD}%s${NC}\n" "$INITIAL_SETUP_TOKEN"
    fi
}

do_update() {
    local current_binary="${INSTALL_DIR}/${BIN_NAME}" current_version latest_version should_stop_after=0 tmp_dir
    [ -x "$current_binary" ] || fail "Meridian 尚未安装，请先运行 install"
    need_cmd curl
    need_cmd tar
    need_cmd mktemp
    init_privilege
    is_systemd && [ ! -f "$SERVICE_FILE" ] \
        && fail "找不到 Meridian systemd 服务，请重新运行 install 修复安装"
    current_version=$(get_current_version)
    latest_version=$(resolve_latest_version)
    if [ "$current_version" = "$latest_version" ]; then
        ok "当前已是最新版本: $latest_version"
        return 0
    fi

    tmp_dir=$(mktemp -d)
    chmod 0700 "$tmp_dir"
    UPDATE_TMP_DIR="$tmp_dir"
    download_release_binary "$latest_version" "$tmp_dir"
    prepare_data_and_config "$tmp_dir"

    UPDATE_TRANSACTION=1
    trap cleanup_update_transaction EXIT
    trap abort_update_transaction INT TERM

    if is_systemd; then
        if service_is_active; then
            UPDATE_WAS_ACTIVE=1
        else
            should_stop_after=1
        fi
        as_root systemctl stop "$SERVICE_NAME"
    else
        ensure_no_manual_process
    fi
    if ! create_backup_archive "pre-${latest_version}"; then
        fail "升级前一致性备份失败，现有程序未被替换"
    fi
    ok "升级前备份已创建: $LAST_BACKUP_PATH"

    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$current_binary" "${PREVIOUS_BIN}.new"
    as_root mv -f "${PREVIOUS_BIN}.new" "$PREVIOUS_BIN"
    as_root install -o root -g "$ROOT_GROUP" -m 0755 "$DOWNLOADED_BINARY" "${current_binary}.new"
    as_root mv -f "${current_binary}.new" "$current_binary"
    UPDATE_BINARY_CHANGED=1

    if is_systemd; then
        as_root systemctl restart "$SERVICE_NAME"
        if ! wait_for_health 20; then
            warn "新版本健康检查失败，正在自动回滚..."
            restore_previous_binary
            UPDATE_BINARY_CHANGED=0
            as_root systemctl restart "$SERVICE_NAME"
            wait_for_health 20 || fail "新版本与回滚版本均未通过健康检查"
            fail "新版本启动失败，已恢复上一版本"
        fi
        if [ "$should_stop_after" = "1" ]; then
            as_root systemctl stop "$SERVICE_NAME"
        fi
    fi

    UPDATE_TRANSACTION=0
    UPDATE_BINARY_CHANGED=0
    UPDATE_TMP_DIR=""
    rm -rf -- "$tmp_dir"
    trap - EXIT INT TERM
    ok "已更新到最新版本: $latest_version"
    info "现有 .env、面板域名、Nginx 配置和证书均已保留"
}

password_byte_length() {
    LC_ALL=C printf '%s' "$1" | wc -c | tr -d '[:space:]'
}

snapshot_auth_files() {
    local snapshot_dir="$1" db_path="$2" source suffix name
    as_root install -d -o root -g "$ROOT_GROUP" -m 0700 "$snapshot_dir"
    as_root cp -p -- "$(env_file_path)" "${snapshot_dir}/env"
    for suffix in "" "-wal" "-shm" "-journal"; do
        source="${db_path}${suffix}"
        name="db${suffix}"
        if as_root test -e "$source"; then
            as_root cp -p -- "$source" "${snapshot_dir}/${name}"
        fi
    done
}

archive_auth_snapshot() {
    local snapshot_dir="$1" stamp archive archive_tmp
    validate_backup_dir
    stamp=$(date -u +%Y%m%dT%H%M%SZ)
    archive="${BACKUP_DIR}/${BIN_NAME}-pre-password-${stamp}-$$.tar.gz"
    archive_tmp="${archive}.tmp.$$"
    as_root install -d -o root -g "$ROOT_GROUP" -m 0700 "$BACKUP_DIR"
    as_root tar -C "$snapshot_dir" -czf "$archive_tmp" . || { as_root rm -f -- "$archive_tmp"; return 1; }
    as_root chmod 0600 "$archive_tmp"
    as_root mv -f "$archive_tmp" "$archive"
    LAST_BACKUP_PATH="$archive"
}

restore_auth_snapshot() {
    local snapshot_dir="$1" db_path="$2" suffix name source
    as_root rm -f -- "$db_path" "${db_path}-wal" "${db_path}-shm" "${db_path}-journal"
    for suffix in "" "-wal" "-shm" "-journal"; do
        name="db${suffix}"
        source="${snapshot_dir}/${name}"
        if as_root test -e "$source"; then
            as_root cp -p -- "$source" "${db_path}${suffix}"
        fi
    done
    as_root cp -p -- "${snapshot_dir}/env" "$(env_file_path)"
}

fix_database_permissions() {
    local db_path="$1" suffix file
    for suffix in "" "-wal" "-shm" "-journal"; do
        file="${db_path}${suffix}"
        if as_root test -e "$file"; then
            as_root chown root:"$ROOT_GROUP" "$file"
            as_root chmod 0600 "$file"
        fi
    done
}

cleanup_password_transaction() {
    local exit_code=$?
    if [ "$exit_code" -ne 0 ] && [ "$PASSWORD_TRANSACTION" = "1" ] \
        && [ -n "$PASSWORD_SNAPSHOT_DIR" ] && [ -n "$PASSWORD_DB_PATH" ]; then
        warn "密码修改中断，正在恢复旧密码和 JWT 配置..."
        as_root systemctl stop "$SERVICE_NAME" >/dev/null 2>&1 || true
        if ! restore_auth_snapshot "$PASSWORD_SNAPSHOT_DIR" "$PASSWORD_DB_PATH" >/dev/null 2>&1; then
            warn "自动恢复凭据失败，请使用备份手动恢复: ${LAST_BACKUP_PATH:-<unknown>}"
        fi
        fix_database_permissions "$PASSWORD_DB_PATH" >/dev/null 2>&1 || true
        if ! as_root systemctl restart "$SERVICE_NAME" >/dev/null 2>&1 \
            || ! wait_for_health 20 >/dev/null 2>&1; then
            warn "凭据已尝试恢复，但 Meridian 未通过健康检查，请检查服务日志"
        fi
        PASSWORD_TRANSACTION=0
    fi
    if [ -n "$PASSWORD_TMP_DIR" ] && [ -d "$PASSWORD_TMP_DIR" ] && [ "$PASSWORD_TMP_DIR" != "/" ]; then
        as_root rm -rf -- "$PASSWORD_TMP_DIR"
    fi
    unset password password_again new_secret 2>/dev/null || true
    return "$exit_code"
}

abort_password_transaction() {
    trap - INT TERM
    exit 130
}

do_password() {
    local password password_again length db_path tmp_dir snapshot_dir rotated_env new_secret mutated=0
    local current_binary="${INSTALL_DIR}/${BIN_NAME}"
    [ -x "$current_binary" ] || fail "Meridian 尚未安装"
    is_systemd || fail "自动修改密码要求 Meridian 由 systemd 管理"
    init_privilege
    [ -f "$SERVICE_FILE" ] || fail "找不到 Meridian systemd 服务，请重新运行 install 修复安装"
    need_cmd tar
    IFS= read -r -s -p "请输入新管理员密码（12-72 字节）: " password
    printf '\n'
    IFS= read -r -s -p "请再次输入新密码: " password_again
    printf '\n'
    [ "$password" = "$password_again" ] || { unset password password_again; fail "两次输入的密码不一致"; }
    length=$(password_byte_length "$password")
    if [ "$length" -lt 12 ] || [ "$length" -gt 72 ]; then
        unset password password_again
        fail "密码必须为 12-72 字节"
    fi

    db_path=$(read_env_value DB_PATH)
    [ -n "$db_path" ] || db_path="${DATA_DIR}/meridian.db"
    validate_db_path "$db_path" || fail "DB_PATH 不是安全的绝对数据库路径"
    as_root test -L "$db_path" && fail "拒绝修改符号链接形式的数据库"
    as_root test -f "$db_path" || fail "数据库不存在: $db_path"

    tmp_dir=$(mktemp -d)
    chmod 0700 "$tmp_dir"
    snapshot_dir="${tmp_dir}/snapshot"
    rotated_env="${tmp_dir}/env.rotated"
    PASSWORD_TMP_DIR="$tmp_dir"
    PASSWORD_SNAPSHOT_DIR="$snapshot_dir"
    PASSWORD_DB_PATH="$db_path"
    as_root systemctl stop "$SERVICE_NAME"
    if ! snapshot_auth_files "$snapshot_dir" "$db_path" || ! archive_auth_snapshot "$snapshot_dir"; then
        as_root systemctl start "$SERVICE_NAME" >/dev/null 2>&1 || true
        as_root rm -rf -- "$tmp_dir"
        unset password password_again
        fail "密码修改前备份失败，未修改任何凭据"
    fi
    ok "凭据备份已创建: $LAST_BACKUP_PATH"
    PASSWORD_TRANSACTION=1
    trap cleanup_password_transaction EXIT
    trap abort_password_transaction INT TERM

    new_secret=$(generate_secret)
    write_rotated_env "$new_secret" "$rotated_env"
    if printf '%s\n' "$password" | as_root "$current_binary" admin reset-password --db "$db_path" --password-stdin; then
        mutated=1
    fi
    unset password password_again
    if [ "$mutated" != "1" ]; then
        fail "管理员密码修改失败，将自动恢复旧密码与 JWT 配置"
    fi

    if ! install_env_file "$rotated_env" || ! fix_database_permissions "$db_path" \
        || ! as_root systemctl restart "$SERVICE_NAME" || ! wait_for_health 20; then
        warn "重启或健康检查失败，正在恢复旧密码与 JWT 配置..."
        fail "密码修改失败，将自动执行凭据回滚"
    fi

    PASSWORD_TRANSACTION=0
    PASSWORD_TMP_DIR=""
    PASSWORD_SNAPSHOT_DIR=""
    PASSWORD_DB_PATH=""
    as_root rm -rf -- "$tmp_dir"
    trap - EXIT INT TERM
    unset new_secret
    ok "管理员密码已修改，所有旧登录令牌已失效"
}

do_uninstall() {
    local remove_data="$PURGE_DATA"
    init_privilege
    warn "即将卸载 Meridian；备份不会删除"
    if [ "$ASSUME_YES" != "1" ]; then
        if [ "$PURGE_DATA" = "1" ]; then
            warn "已指定 --purge，数据目录将在确认卸载后删除: $DATA_DIR"
        else
            if ask_yes_no "是否同时删除数据目录 ${DATA_DIR}（数据库和密钥）？" 0; then
                remove_data=1
            fi
        fi
        ask_yes_no "确认卸载 Meridian？" 0 || { info "已取消"; return 0; }
    fi

    [ "$remove_data" = "0" ] || validate_data_dir

    if is_systemd && [ -f "$SERVICE_FILE" ]; then
        as_root systemctl stop "$SERVICE_NAME" 2>/dev/null || true
        as_root systemctl disable "$SERVICE_NAME" 2>/dev/null || true
        as_root rm -f -- "$SERVICE_FILE"
        as_root systemctl daemon-reload
    fi
    as_root rm -f -- "${INSTALL_DIR}/${BIN_NAME}" "$PREVIOUS_BIN" \
        "${INSTALL_DIR}/${BIN_NAME}.new" "${INSTALL_DIR}/${BIN_NAME}.rollback"

    if [ "$remove_data" = "1" ]; then
        as_root rm -rf -- "$DATA_DIR"
        ok "数据目录已删除；备份目录仍保留: $BACKUP_DIR"
    else
        info "数据目录已保留: $DATA_DIR"
    fi
    ok "Meridian 已卸载"
}

usage() {
    cat <<'USAGE'
Meridian 一键安装工具

用法:
  install.sh install [-y]
      首次安装最新版本。
  install.sh update [-y]
      更新到最新 Release，自动备份、健康检查并在失败时回滚。
  install.sh password
      隐藏输入并修改唯一管理员密码，同时轮换 JWT 密钥。
  install.sh uninstall [-y] [--purge]
      卸载程序；默认保留数据与备份，--purge 才删除数据。
  install.sh help
      显示本帮助。

选项:
  -y, --yes    非交互确认
  --purge      卸载时删除数据目录；不会删除备份

反向代理请参考 docs/nginx-site.conf 自行配置。
不带参数运行时进入菜单。
USAGE
}

main_menu() {
    local current choice
    current=$(get_current_version)
    printf '\n%s\n' "Meridian 一键安装工具"
    printf '  当前版本: %s\n\n' "${current:-未安装}"
    printf '  1) 安装\n'
    printf '  2) 更新到最新版\n'
    printf '  3) 修改管理员密码\n'
    printf '  4) 卸载\n'
    printf '  0) 退出\n\n'
    read -r -p "请选择 [0-4]: " choice
    case "$choice" in
        1) do_install ;;
        2) do_update ;;
        3) do_password ;;
        4) do_uninstall ;;
        0) exit 0 ;;
        *) fail "无效选项" ;;
    esac
}

run_cli() {
    local action="${1:-menu}"
    [ "$#" -eq 0 ] || shift
    case "$action" in -h|--help) action="help" ;; esac

    while [ "$#" -gt 0 ]; do
        case "$1" in
            -y|--yes) ASSUME_YES=1 ;;
            --purge)
                [ "$action" = "uninstall" ] || fail "--purge 仅用于 uninstall"
                PURGE_DATA=1
                ;;
            -h|--help) action="help" ;;
            *) fail "未知参数: $1" ;;
        esac
        shift
    done

    case "$action" in
        install) do_install ;;
        update) do_update ;;
        password) do_password ;;
        uninstall) do_uninstall ;;
        help) usage ;;
        menu) main_menu ;;
        *) fail "未知操作: $action（仅支持 install、update、password、uninstall、help）" ;;
    esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
    run_cli "$@"
fi
