#!/bin/bash

# DownOnly 自动安装脚本
# 仓库: https://github.com/EchoPing07/DownOnly

set -e

# ========== 配置 ==========
REPO="EchoPing07/DownOnly"
APP_DIR="/root/downonly"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"

# ========== 颜色 ==========
G='\033[0;32m'
R='\033[0;31m'
Y='\033[0;33m'
B='\033[1;34m'
W='\033[0m'

# ========== 权限检查 ==========
if [ "$EUID" -ne 0 ]; then 
    echo -e "${R}错误: 请使用 root 权限运行${W}"
    exit 1
fi

# ========== 欢迎信息 ==========
clear
echo -e "${B}"
echo " ______   _______  _     _  __    _  _______  __    _  ___      __   __ "
echo "|      | |       || | _ | ||  |  | ||       ||  |  | ||   |    |  | |  |"
echo "|  _    ||   _   || || || ||   |_| ||   _   ||   |_| ||   |    |  |_|  |"
echo "| | |   ||  | |  ||       ||       ||  | |  ||       ||   |    |       |"
echo "| |_|   ||  |_|  ||       ||  _    ||  |_|  ||  _    ||   |___ |_     _|"
echo "|       ||       ||   _   || | |   ||       || | |   ||       |  |   |  "
echo "|______| |_______||__| |__||_|  |__||_______||_|  |__||_______|  |___|  "
echo -e "${W}"
echo -e "${Y} DownOnly 自动安装程序${W}"
echo ""

# ========== 检测架构 ==========
echo -e "${B}[1/9]${W} 检测系统架构..."
ARCH=$(uname -m)
case $ARCH in
    x86_64) GOARCH="amd64" ;;
    aarch64) GOARCH="arm64" ;;
    *)
        echo -e "${R}不支持的架构: $ARCH${W}"
        exit 1
        ;;
esac
echo -e "      架构: ${G}${GOARCH}${W}"

# ========== 获取最新版本 ==========
echo -e "${B}[2/9]${W} 获取最新版本..."
LATEST=$(curl -s "$API_URL" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
if [ -z "$LATEST" ]; then
    echo -e "${R}无法获取版本信息${W}"
    exit 1
fi
echo -e "      版本: ${G}${LATEST}${W}"

# ========== 尝试下载预编译文件 ==========
echo -e "${B}[3/9]${W} 尝试下载预编译文件..."
DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/downonly-linux-${GOARCH}-${LATEST}"

mkdir -p "${APP_DIR}/data"
cd "${APP_DIR}"

if wget -q --show-progress -O downonly "$DOWNLOAD_URL" 2>&1; then
    echo -e "${G}      下载成功${W}"
    # SHA256 校验（如果存在校验文件）
    SHA256_URL="${DOWNLOAD_URL}.sha256"
    if wget -q -O downonly.sha256 "$SHA256_URL" 2>/dev/null; then
        EXPECTED=$(awk '{print $1}' downonly.sha256)
        ACTUAL=$(sha256sum downonly | awk '{print $1}')
        if [ "$EXPECTED" = "$ACTUAL" ]; then
            echo -e "${G}      SHA256 校验通过${W}"
        else
            echo -e "${R}      SHA256 校验失败，文件可能被篡改${W}"
            rm -f downonly downonly.sha256
            exit 1
        fi
        rm -f downonly.sha256
    else
        echo -e "${Y}      未找到校验文件，跳过校验${W}"
    fi
    chmod +x downonly
else
    echo -e "${Y}      预编译文件不存在，准备本地编译...${W}"
    
    # ========== 检查 Go 环境 ==========
    echo -e "${B}[4/9]${W} 检查 Go 环境..."
    if ! command -v go &> /dev/null; then
        echo -e "${Y}      未检测到 Go，开始安装依赖...${W}"
        
        if command -v apt &> /dev/null; then
            apt update -qq && apt install -y wget tar git > /dev/null 2>&1
        elif command -v yum &> /dev/null; then
            yum install -y wget tar git > /dev/null 2>&1
        elif command -v apk &> /dev/null; then
            apk add wget tar git > /dev/null 2>&1
        else
            echo -e "${R}      无法检测包管理器，请手动安装 wget, tar, git${W}"
            exit 1
        fi
        
        GO_VERSION="1.22.5"
        GO_FILE="go${GO_VERSION}.linux-${GOARCH}.tar.gz"
        
        cd /tmp
        GO_URL_CN="https://golang.google.cn/dl/${GO_FILE}"
        GO_URL_OFFICIAL="https://go.dev/dl/${GO_FILE}"
        if ! wget -q --show-progress -O "${GO_FILE}" "${GO_URL_CN}" 2>&1; then
            echo -e "${Y}      国内源下载失败，尝试官方源...${W}"
            wget -q --show-progress -O "${GO_FILE}" "${GO_URL_OFFICIAL}" 2>&1
        fi
        tar -C /usr/local -xzf "${GO_FILE}"
        rm -f "${GO_FILE}"
        
        export PATH=$PATH:/usr/local/go/bin
        export GOPATH=/root/go
        export GOCACHE=/tmp/go-cache
        
        echo -e "${G}      Go 安装完成${W}"
    else
        echo -e "${G}      Go 已安装${W}"
    fi
    
    # ========== 本地编译 ==========
    echo -e "${B}[5/9]${W} 开始编译（约需 2-3 分钟）..."
    cd /tmp
    rm -rf /tmp/downonly-build
    git clone --depth 1 "https://github.com/${REPO}.git" /tmp/downonly-build
    cd /tmp/downonly-build
    go build -ldflags="-s -w" -o "${APP_DIR}/downonly" .
    cd "${APP_DIR}"
    rm -rf /tmp/downonly-build
    echo -e "${G}      编译完成${W}"
fi

# ========== 安装管理脚本 ==========
echo -e "${B}[6/9]${W} 安装管理脚本..."
cat > /usr/local/bin/downonly << 'MANAGER_SCRIPT'
#!/bin/bash

APP_DIR="/root/downonly"
SERVICE="downonly"
REPO="EchoPing07/DownOnly"
API_URL="https://api.github.com/repos/${REPO}/releases/latest"

G='\033[0;32m'
R='\033[0;31m'
W='\033[0m'
B='\033[1;34m'
Y='\033[0;33m'

[[ $EUID -ne 0 ]] && echo -e "${R} 错误: 请使用 root 权限${W}" && exit 1

get_arch() {
    case $(uname -m) in
        x86_64) echo "amd64" ;;
        aarch64) echo "arm64" ;;
        *) echo "unknown" ;;
    esac
}

get_status() {
    if systemctl is-active "$SERVICE" >/dev/null 2>&1; then
        echo -e "${G}运行中${W}"
    else
        echo -e "${R}已停止${W}"
    fi
}

show_menu() {
    clear
    echo -e "${B}"
    echo " ______   _______  _     _  __    _  _______  __    _  ___      __   __ "
    echo "|      | |       || | _ | ||  |  | ||       ||  |  | ||   |    |  | |  |"
    echo "|  _    ||   _   || || || ||   |_| ||   _   ||   |_| ||   |    |  |_|  |"
    echo "| | |   ||  | |  ||       ||       ||  | |  ||       ||   |    |       |"
    echo "| |_|   ||  |_|  ||       ||  _    ||  |_|  ||  _    ||   |___ |_     _|"
    echo "|       ||       ||   _   || | |   ||       || | |   ||       |  |   |  "
    echo "|______| |_______||__| |__||_|  |__||_______||_|  |__||_______|  |___|  "
    echo -e "${W}"
    echo -e " 状态: $(get_status)"
    echo ""
    echo " ┌─────────────────────────────────────────────────┐"
    echo " │  1. 启动   2. 停用   3. 重启   4. 日志          │"
    echo " │                                                 │"
    echo " │  5. 更新   6. 卸载   0. 退出                    │"
    echo " └─────────────────────────────────────────────────┘"
    echo ""
}

do_update() {
    echo -e "${Y} 正在检查最新版本...${W}"
    
    LATEST=$(curl -s "$API_URL" | grep '"tag_name"' | sed -E 's/.*"([^"]+)".*/\1/')
    if [ -z "$LATEST" ]; then
        echo -e "${R} 无法获取版本信息，请检查网络${W}"
        sleep 2
        return 1
    fi
    
    echo -e " 最新版本: ${G}${LATEST}${W}"
    read -p " 是否更新? (y/n): " confirm
    [[ $confirm != "y" ]] && return 0
    
    ARCH=$(get_arch)
    if [ "$ARCH" == "unknown" ]; then
        echo -e "${R} 不支持的架构${W}"
        sleep 2
        return 1
    fi
    
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${LATEST}/downonly-linux-${ARCH}-${LATEST}"
    
    echo -e " 正在下载..."
    systemctl stop $SERVICE
    
    if wget -q --show-progress -O "${APP_DIR}/downonly.new" "$DOWNLOAD_URL"; then
        # SHA256 校验（如果存在）
        SHA256_URL="${DOWNLOAD_URL}.sha256"
        if wget -q -O "${APP_DIR}/downonly.new.sha256" "$SHA256_URL" 2>/dev/null; then
            EXPECTED=$(awk '{print $1}' "${APP_DIR}/downonly.new.sha256")
            ACTUAL=$(sha256sum "${APP_DIR}/downonly.new" | awk '{print $1}')
            if [ "$EXPECTED" != "$ACTUAL" ]; then
                echo -e "${R} SHA256 校验失败${W}"
                rm -f "${APP_DIR}/downonly.new" "${APP_DIR}/downonly.new.sha256"
                systemctl start $SERVICE
                sleep 2
                return 1
            fi
            rm -f "${APP_DIR}/downonly.new.sha256"
        fi
        chmod +x "${APP_DIR}/downonly.new"
        mv "${APP_DIR}/downonly.new" "${APP_DIR}/downonly"
        systemctl start $SERVICE
        echo -e "${G} 更新成功！${W}"
    else
        echo -e "${R} 下载失败，尝试回滚...${W}"
        rm -f "${APP_DIR}/downonly.new"
        if [ -f "${APP_DIR}/downonly" ] && [ -x "${APP_DIR}/downonly" ]; then
            systemctl start $SERVICE
            echo -e "${Y} 已回滚到旧版本并重启服务${W}"
        else
            echo -e "${R} 无可用版本，请手动处理${W}"
        fi
    fi
    
    sleep 2
}

while true; do
    show_menu
    read -p " 输入选项: " opt
    case $opt in
        1) systemctl start $SERVICE && echo -e " 已启动" && sleep 1 ;;
        2) systemctl stop $SERVICE && echo -e " 已停止" && sleep 1 ;;
        3) systemctl restart $SERVICE && echo -e " 已重启" && sleep 1.5 ;;
        4) clear && echo -e "${Y} [按 Ctrl+C 返回菜单] ${W}" && echo "" && tail -n 100 -f "${APP_DIR}/data/sys_out.log" 2>/dev/null || true ;;
        5) do_update ;;
        6) echo ""; read -p " 是否保留数据? (y保留/n全删): " keep
           systemctl stop $SERVICE &>/dev/null
           systemctl disable $SERVICE &>/dev/null
           rm -f /etc/systemd/system/$SERVICE.service
           systemctl daemon-reload
           if [[ $keep == "y" ]]; then
               rm -f "${APP_DIR}/downonly"
               echo -e "${G} 已卸载，数据已保留${W}"
           else
               rm -rf "${APP_DIR}"
               echo -e "${G} 已卸载并清除所有数据${W}"
           fi
           rm -f /usr/local/bin/downonly
           exit 0 ;;
        0) exit 0 ;;
        *) echo -e " 无效选项" && sleep 1 ;;
    esac
done
MANAGER_SCRIPT

chmod +x /usr/local/bin/downonly
echo -e "${G}      完成${W}"

# ========== 配置 systemd 服务 ==========
echo -e "${B}[7/9]${W} 配置系统服务..."
cat > /etc/systemd/system/downonly.service << 'SERVICE_FILE'
[Unit]
Description=DownOnly Traffic Guard
After=network.target

[Service]
WorkingDirectory=/root/downonly
ExecStart=/root/downonly/downonly
Restart=always
RestartSec=5
StandardOutput=append:/root/downonly/data/sys_out.log
StandardError=append:/root/downonly/data/sys_err.log

[Install]
WantedBy=multi-user.target
SERVICE_FILE

systemctl daemon-reload
systemctl enable downonly
systemctl start downonly
echo -e "${G}      完成${W}"

# ========== 配置日志轮转 ==========
echo -e "${B}[8/9]${W} 配置日志轮转..."
cat > /etc/logrotate.d/downonly << 'LOGROTATE'
/root/downonly/data/sys_out.log
/root/downonly/data/sys_err.log
{
    daily
    rotate 7
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
    maxsize 50M
}
LOGROTATE
echo -e "${G}      完成${W}"

# ========== 完成 ==========
echo -e "${B}[9/9]${W} 安装完成！"
echo ""
echo -e "${G}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${W}"
IP=$(hostname -I | awk '{print $1}')
echo -e " 访问地址: ${Y}http://${IP}:8080${W}"
echo -e " 管理命令: ${Y}downonly${W}"
echo -e "${G}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${W}"
echo ""
