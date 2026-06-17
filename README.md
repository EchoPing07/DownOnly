# DownOnly
**一款专为增加下行流量设计的轻量级工具**

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://golang.org)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Platform](https://img.shields.io/badge/Platform-Linux-blue)](https://github.com/EchoPing07/DownOnly/releases)

---

## 📖 项目背景

在当前宽带运营环境下，许多 **NAS 用户、PT 玩家或自建服务用户** 由于上传流量与下载流量比例异常，容易被运营商的大数据算法误判为"商业 PCDN"行为，导致：
- 🚫 被限速甚至停网
- 📝 强制要求签订保证书
- 📞 频繁接到"警告"电话

**DownOnly** 通过产生真实但"无害"的下载流量，在不损耗硬件寿命的前提下，平衡你的宽带上下行比例，规避运营商的弱智判断算法。

> **声明**：我不推荐任何浪费公共网络资源的行为，但也不希望自身合法合规的行为被套上莫须有的罪名。

---

## ✨ 核心特性

### 🛡️ 零损耗设计
- **黑洞机制**：采用 `io.Discard` 技术，下载数据在内存中直接丢弃，**0 磁盘写入**
- **闪存友好**：保护 eMMC、SSD 等存储介质的硬件寿命
- **低内存占用**：运行时仅占用 **6-15MB** 内存，512MB 设备也能流畅运行
- **低优先级运行**：Linux 下自动设置进程优先级为最低（nice 19），避免影响其他服务
- **GC 优化**：`GOGC=200` 降低 GC 频率，进一步减少资源开销

### 📊 专业审计系统
- **实时监控**：Mbps 级速率统计 + 30 秒实时折线图
- **流量统计**：按日记录流量并以柱状图展示，支持按月查看历史
- **数据持久化**：自动归档本年度数据，跨年自动清理旧数据
- **日志管理**：按天归档日志文件，每日上限 1000 条，自动清理 7 天前的过期日志

### 🧠 智能调度引擎
- **时间窗口**：自定义每日运行时间段（如仅在 18:00 - 23:00 运行），支持跨午夜设置
- **限额保护**：设置每日流量限额区间，每天自动随机生成实际限额值，达到后自动待机至次日
- **伪装技术**：内置 4 种主流浏览器 User-Agent 自动轮换 + 多源随机切换，模拟正常用户行为
- **休息间隔**：自定义两次下载任务之间的随机等待时长的范围，也可禁用休息实现连续下载
- **多源容错**：多个下载地址自动随机顺序尝试，单地址失败自动重试，全部失败后进入休息
- **状态恢复**：程序重启后自动恢复上次运行状态，无需手动重新启动

### 🔒 安全防护
- **公网访问控制**：默认仅允许局域网访问，需手动开启公网访问权限
- **SSRF 防护**：下载地址校验机制，禁止访问内网/私有地址，DNS 解析后二次校验防止 DNS Rebinding 攻击
- **优雅退出**：捕获 SIGINT/SIGTERM 信号，退出前自动保存所有数据

### 🎨 现代化 WebUI
- **深色/浅色模式**：自动跟随系统主题，也可手动切换并持久化偏好
- **响应式设计**：完美适配手机、平板、PC
- **交互友好**：所有配置可在网页端实时修改，无需重启
- **地址管理**：支持添加/删除/批量测试下载源可达性
- **日志查看器**：支持按日期切换历史日志，实时自动刷新，点击展开详情

---

## 🚀 部署指南

### 方式一：一键安装（推荐）

**适用于 Linux 系统（x86_64 / ARM64 / ARMv7 / ARMv6），自动处理所有依赖和配置。**

```bash
curl -fsSL https://raw.githubusercontent.com/EchoPing07/DownOnly/main/install.sh | bash
```

**安装过程说明：**
1. 自动检测系统架构
2. 从 GitHub Release 下载预编译文件（自动 SHA256 校验，若无则本地编译）
3. 配置 systemd 服务
4. 安装管理脚本到系统

**安装完成后：**
- 访问地址：`http://你的设备IP:8080`
- 管理命令：直接输入 `downonly` 呼出可视化管理菜单

**管理菜单选项：**
- `1` - 启动服务
- `2` - 停止服务
- `3` - 重启服务
- `4` - 查看实时日志
- `5` - 更新至最新版本
- `6` - 卸载程序
- `0` - 退出菜单

---

### 方式二：手动部署

#### 1. 前置准备

**检查端口占用**（DownOnly 默认使用 `8080` 端口）：
```bash
# 检查 8080 端口是否被占用
netstat -tuln | grep 8080
# 或
lsof -i:8080

# 如果有输出，说明端口被占用，请先关闭占用进程或修改 main.go 中的端口号
```

**安装 Go 环境**（如果未安装）：
```bash
# 下载 Go（以 ARM64 为例）
wget https://golang.google.cn/dl/go1.22.5.linux-arm64.tar.gz
sudo tar -C /usr/local -xzf go1.22.5.linux-arm64.tar.gz

# 配置环境变量
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc

# 验证安装
go version
```

#### 2. 编译程序

```bash
# 克隆仓库
git clone https://github.com/EchoPing07/DownOnly.git
cd DownOnly

# 整理依赖
go mod tidy

# 编译（不要只指定 main.go，需要包含同目录下所有源文件）
go build -ldflags="-s -w" -o downonly

# 创建数据目录
mkdir -p data
```

#### 3. 配置系统服务

```bash
# 创建 systemd 服务文件
sudo tee /etc/systemd/system/downonly.service > /dev/null <<EOF
[Unit]
Description=DownOnly Traffic Guard
After=network.target

[Service]
WorkingDirectory=$(pwd)
ExecStart=$(pwd)/downonly
Restart=always
RestartSec=5
StandardOutput=append:$(pwd)/data/sys_out.log
StandardError=append:$(pwd)/data/sys_err.log

[Install]
WantedBy=multi-user.target
EOF

# 重载并启动服务
sudo systemctl daemon-reload
sudo systemctl enable downonly
sudo systemctl start downonly

# 查看运行状态
sudo systemctl status downonly
```

#### 4. 访问 Web 界面

在浏览器中输入：`http://你的设备IP:8080`

---
### 方式三：Docker 部署

**GHCR** 地址：
https://github.com/EchoPing07/DownOnly/pkgs/container/downonly

**Docker Hub** 地址：
https://hub.docker.com/r/echoping/downonly

以下模板使用 **GHCR** 仓库地址演示。

> **时区说明**：镜像已内置 `Asia/Shanghai`（中国标准时间）时区，**中国用户无需额外配置**。其他时区用户请添加 `-v /etc/localtime:/etc/localtime:ro` 参数以同步宿主机时区。

**Dockerfile 构建说明**：项目采用多阶段构建，`index.html` 和 `icons/` 目录在编译时通过 Go 的 `embed` 机制嵌入二进制文件，运行时无需额外挂载。仅需挂载 `data/` 目录持久化数据。

#### Docker Run 运行：

```bash
# 中国用户（镜像默认中国时区）
docker run -d \
  --name downonly \
  -p 8080:8080 \
  -v /opt/downonly/data:/app/data \
  --restart always \
  ghcr.io/echoping07/downonly:latest

# 其他时区用户（需同步宿主机时区）
docker run -d \
  --name downonly \
  -p 8080:8080 \
  -v /opt/downonly/data:/app/data \
  -v /etc/localtime:/etc/localtime:ro \
  --restart always \
  ghcr.io/echoping07/downonly:latest
```

#### Docker Compose 运行：

创建 `docker-compose.yml` 文件：
```yaml
services:
  downonly:
    image: ghcr.io/echoping07/downonly:latest
    container_name: downonly
    ports:
      - "8080:8080"
    volumes:
      - ./data:/app/data
      # 其他时区用户取消注释下行以同步宿主机时区
      # - /etc/localtime:/etc/localtime:ro
    restart: always
```
运行命令：
```bash
docker-compose up -d
```

---

## 📂 目录结构

```
DownOnly/
├── downonly                # 主程序二进制文件
├── main.go                 # 后端源代码
├── priority_linux.go       # Linux 低优先级设置
├── priority_other.go       # 非 Linux 平台
├── index.html              # 前端 WebUI
├── Dockerfile              # Docker 多阶段构建配置
├── icons/                  # SVG 图标资源
└── data/                   # 数据目录
    ├── config.json         # 用户配置
    ├── stats.json          # 流量统计数据
    └── logs/               # 按天归档的日志目录
        ├── log-2025-05-01.json
        └── log-2025-05-02.json
```

---

## 🛠️ 进阶配置

### 1. 修改下载源

默认使用 Apple CDN 地址，建议修改为运营商自有 CDN 或大厂提供的测速文件。

在 WebUI 的"地址管理"中添加下载直链（支持批量测试可达性）。例如：
```
http://updates-http.cdn-apple.com/...
https://releases.ubuntu.com/...
https://speed.cloudflare.com/__down?bytes=1000000000
```

> **安全限制**：仅允许 HTTP/HTTPS 协议的公网地址，自动屏蔽内网/私有 IP，防止 SSRF 攻击。

### 2. 限速策略建议

默认下载速度为 **5Mbps**，WebUI 滑块支持 1~256 Mbps，程序实际校验范围为 1~1000 Mbps。建议根据实际带宽设定，一般设为总带宽的 10%~20% 即可

### 3. 时间窗口设置

可以设置仅在半夜时段运行，避免和其他设备抢占网络资源。

### 4. 端口修改

如果 `8080` 端口被占用，可以在启动时指定端口：
```bash
./downonly 9999  # 使用 9999 端口
```

或修改 `main.go` 中的 `port := "8080"` 行。

### 5. 默认配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `speed_limit_mbps` | 5 | 下载速率限制（Mbps），范围 1~1000 |
| `daily_quota_min_gb` | 150 | 每日流量限额最小值（GB），范围 1~99999 |
| `daily_quota_max_gb` | 200 | 每日流量限额最大值（GB），范围 1~99999 |
| `schedule_start` | 00:00 | 每日运行开始时间（HH:MM） |
| `schedule_end` | 23:59 | 每日运行结束时间（HH:MM） |
| `sleep_min_minutes` | 10 | 休息间隔最小值（分钟），范围 0~1440 |
| `sleep_max_minutes` | 20 | 休息间隔最大值（分钟），范围 0~1440 |
| `sleep_disabled` | false | 是否禁用休息间隔 |
| `allow_public_access` | false | 是否允许公网设备访问 WebUI |
| `urls` | Apple CDN | 下载地址列表 |

### 6. HTTP API 接口

所有接口默认仅允许局域网访问（开启公网访问后除外）：

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/` | WebUI 页面 |
| GET | `/api/status` | 获取运行状态、速率、流量、运行时长 |
| POST | `/api/toggle` | 切换启停状态 |
| GET | `/api/history?month=N` | 获取指定月份每日流量数据 |
| GET | `/api/log_dates` | 获取所有可用日志日期列表 |
| GET | `/api/logs?date=YYYY-MM-DD` | 获取指定日期的日志条目 |
| GET | `/api/config` | 获取当前配置 |
| POST | `/api/config` | 更新配置（JSON Body） |
| GET | `/api/test_url?url=...` | 测试下载地址可达性 |

---

## 🖥️ 已测试设备

- ✅ 骁龙 410 棒子 (512MB 内存) - Debian 11
- ✅ 网心云 OEC (2GB 内存) - fnOS 1.1.19

---

## 🔧 故障排查

### 1. 无法访问 WebUI
```bash
# 检查服务状态
systemctl status downonly

# 查看日志
tail -f /root/downonly/data/sys_err.log

# 检查端口是否被占用
netstat -tuln | grep 8080

# 检查是否开启了公网访问（默认仅允许局域网访问）
# 如果从公网访问，需在 WebUI 策略配置中开启"允许公网访问"
```

### 2. 流量统计不准确
- 检查系统时间是否正确（`date`）
- 确认 `data/stats.json` 文件权限正常
- 流量统计基于实际网络传输字节数，每日 00:00 自动重置

### 3. 内存占用过高
- 检查是否有多个实例在运行（`ps aux | grep downonly`）
- 降低下载速率限制
- 程序已设置 `GOGC=200` 和 Linux 最低进程优先级，正常使用无需额外调整

### 4. 下载地址不可用
- 使用 WebUI 中的"批量测试"功能检查所有地址可达性
- 确认地址为公网 HTTP/HTTPS 直链，内网地址会被安全策略自动拦截
- 单个地址失败会自动切换到下一个地址重试

---

## ⚖️ 开源协议

本项目采用 **MIT License** 开源。

---

## ⚠️ 免责声明

1. 本工具**仅用于个人学习编程以及评估宽带网络状况**。
2. 请在当地法律及运营商协议允许范围内使用。
3. 用户需**自行承担**因异常流量可能引发的运营商风险。
4. 作者不对任何因使用本工具导致的直接或间接损失负责。

---

**如果觉得这个项目对你有帮助，请点个 Star ⭐️ 支持一下！**
