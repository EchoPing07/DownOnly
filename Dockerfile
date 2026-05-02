# 第一阶段：编译
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY . .
RUN go mod init downonly || true
RUN go mod tidy
RUN go build -ldflags="-s -w" -o downonly

# 第二阶段：运行
FROM alpine:latest
WORKDIR /app

# 设置中国时区
RUN apk add --no-cache tzdata \
    && cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
    && echo "Asia/Shanghai" > /etc/timezone \
    && apk del tzdata
ENV TZ=Asia/Shanghai

# 复制编译好的二进制文件
COPY --from=builder /app/downonly .
# 复制前端文件
COPY --from=builder /app/index.html .
# 复制图标资源
COPY --from=builder /app/icons ./icons

# 创建数据目录
RUN mkdir -p /app/data

# 开放 8080 端口
EXPOSE 8080

# 运行命令
ENTRYPOINT ["./downonly"]
