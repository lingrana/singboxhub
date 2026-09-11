# sing-box hub

独立的**多节点 sing-box 监控管理面板**:一个面板集中管理多台服务器上的 sing-box 实例,
监测每个节点的**客户端来源 IP** 与**流量消耗**,支持出口 IP 探测与订阅导出。
单二进制交付(纯 Go 实现,UI 由服务端渲染并嵌入二进制),节点侧**无需安装任何组件**。

```
┌────────────┐  Clash API(WS)   ┌──────────────────┐
│ sing-box ① │ ◄──────────────── │                  │
├────────────┤  /traffic         │   sing-box hub   │  HTTP API + Web UI
│ sing-box ② │ ◄──────────────── │   (独立部署)      │  ──► 浏览器
├────────────┤  /connections     │                  │
│ sing-box N │ ◄──────────────── │  SQLite 历史      │
└────────────┘                   └──────────────────┘
```

## 功能

- **多节点管理**:注册任意多个 sing-box(名称/API 地址/secret/标签),健康检查、启用停用
- **流量监测**:实时速率(1s)、分钟/小时/天三级历史曲线与汇总、全节点聚合趋势
- **来源 IP 监测**:每节点按客户端 IP 统计上/下行流量、连接数、活跃数(采样口径,详见下文)
- **出口 IP 探测**:经节点出站请求 IP 归属服务,异步 operation 流程,结果缓存
- **基本控制**:查看/断开连接、切换策略组选择、切换 rule/global/direct 模式(透传 Clash API)
- **订阅导出**:把已配置出站的节点打包为订阅 URL(sing-box JSON / Clash YAML / base64 分享链接)+ 二维码,令牌可轮换/吊销
- **安全**:JWT 会话 + 刷新令牌吊销、登录限流、节点 secret AES-256-GCM 加密落库、problem+json 错误、请求 ID 贯通

### UI 架构(纯 Go 服务端渲染)

Web UI 由 Go `html/template` 服务端渲染 + 内嵌 htmx 实现,**无 Node/npm 构建链**。
浏览器访问 `/login` 和 `/admin/*`,使用 HttpOnly Cookie 会话(自动静默续期);
程序化客户端使用 JSON API(`openapi.yaml` 契约,Bearer 认证),两者共用同一 API。

## 快速开始

### Docker(推荐)

```bash
cd deploy
docker compose up -d
# 初始密码:deploy/data/initial-admin-password.txt(登录后请删除该文件)
# 面板:http://<服务器>:9090
```

每次推送到 `main` 或创建 `v*.*.*` 标签时,GitHub Actions 会构建并发布镜像
`ghcr.io/lingrana/singboxhub`。`deploy/docker-compose.yml` 默认直接拉取
`ghcr.io/lingrana/singboxhub:main`;镜像不包含数据库、密钥或节点配置。

### 二进制 / systemd

```bash
# 只需 Go 1.25+(UI 模板与静态资源已嵌入仓库,无需任何前端工具链)
cd backend && go build -o panel ./cmd/panel

sudo mkdir -p /var/lib/sing-box-hub && sudo useradd -r panel
sudo install -m 755 panel /usr/local/bin/panel
sudo install -m 644 deploy/panel.service /etc/systemd/system/
sudo systemctl enable --now panel
# 初始密码:/var/lib/sing-box-hub/initial-admin-password.txt
```

配置优先级:命令行 flag > 环境变量 > YAML(`-config panel.yaml`) > 默认值。

直接运行程序也会自动创建数据目录、密钥和管理员账号,初始随机密码写入数据目录中的
`initial-admin-password.txt`。已有账号时重启不会重置密码,可删除该文件。
修改用户名或密码会撤销该用户全部会话;删除用户后,其访问令牌和刷新令牌立即失效。
升级后旧访问令牌需通过有效刷新令牌续期,或重新登录。

| Flag | 环境变量 | 默认 | 说明 |
| --- | --- | --- | --- |
| `-listen` | `SINGHUB_LISTEN` | `:9090` | 监听地址 |
| `-data-dir` | `SINGHUB_DATA_DIR` | `data` | 数据目录(SQLite/密钥) |
| `-log-level` | `SINGHUB_LOG_LEVEL` | `info` | debug/info/warn/error |

## 节点侧配置(唯一要求)

每个被管理的 sing-box 只需开启内置 Clash API:

```json
{
  "experimental": {
    "clash_api": {
      "external_controller": "0.0.0.0:9090",
      "secret": "换一个强随机串"
    }
  }
}
```

然后在面板「节点管理 → 添加节点」填入 `http://<节点IP>:9090` 与该 secret。
可选:粘贴该节点的**出站(outbound)JSON**——配置后该节点会出现在订阅里,并支持出口 IP 探测。

> 安全提示:Clash API 本身是明文 HTTP。请确保面板与节点之间的网络可信
> (内网 / VPN / SSH 隧道),或用反代加 TLS。secret 泄露等于节点控制权泄露。
> 面板自身建议置于 HTTPS 反代之后(订阅 URL 含令牌,务必 HTTPS)。

## 出口 IP 探测

面板在本机运行一份 sing-box(官方二进制),按节点出站配置生成临时配置并发起探测。
在「系统设置 → sing-box 二进制路径」填写路径(如 `/usr/local/bin/sing-box`);
未配置二进制或节点未配置出站时,探测功能对该节点不可用,其余功能不受影响。

## 来源 IP 统计口径(best-effort)

面板每 2 秒(可配)采样一次节点 `/connections` 快照,按连接 ID 增量累计到来源 IP。
设置中的 2-60 秒间隔通过 WebSocket 的毫秒 `interval` 参数传给节点,保存后自动重建订阅。
存活时间短于采样间隔的连接可能漏计;流量「总量」以 `/traffic` 流为准,来源 IP 明细为近似值。
在 UI 中已标注该口径。

历史清理在启动时和每小时执行:原始样本保留 3 天,分钟/小时汇总按设置的天数保留,
日汇总和 IP 日用量保留 365 天。设置页的 Logo 和浏览器图标支持 PNG/JPEG/GIF,
最大 2 MiB,宽高不超过 4096 像素;旧版本损坏的图片需重新上传。

## 订阅安全模型

订阅是「能力 URL」:令牌在路径中(`/sub/{token}`),因为客户端 App 无法携带 Authorization 头。
缓解措施:令牌可随时轮换与吊销、端点只读、有限流与访问日志、错误统一 404 不泄露存在性。
详情见 `openapi.yaml` 的 info.description。

## 本地开发

```bash
cd backend && go run ./cmd/panel              # 面板(http://127.0.0.1:9090)
go run ./cmd/mockclash                        # 可选:假节点,无需真实 sing-box 即可体验
```

UI 模板在 `backend/internal/webui/templates/`,静态资源(css/htmx/app.js)在
`backend/internal/webui/static/`,修改后重新 `go build` 即生效。

`cmd/mockclash` 是一个模拟 Clash API 的开发工具(产生随机流量与连接),
把面板节点指向它即可完整体验监控链路。

## 测试与 API 契约

```bash
cd backend && go test ./...
npx @redocly/cli lint openapi.yaml   # 契约 lint
```

API 契约:`openapi.yaml`(OpenAPI 3.2,唯一事实来源)。开发流程见
[docs/plan-generate-review-prompt.md](docs/plan-generate-review-prompt.md)。

## 目录结构

```
├── openapi.yaml            # API 契约(OpenAPI 3.2)
├── docs/                   # 开发工作流文档与计划
├── backend/
│   ├── cmd/panel/          # 服务入口
│   ├── cmd/mockclash/      # 开发用假节点
│   └── internal/
│       ├── api/            # HTTP handlers(JSON API)
│       ├── webui/          # 服务端渲染 UI(模板 + htmx + cookie 会话)
│       ├── auth/           # JWT/密码/限流
│       ├── clash/          # 节点 Clash API 客户端(HTTP+WS)
│       ├── sampler/        # 采集器(每节点双 WS + 落库)
│       ├── store/          # SQLite(WAL)
│       ├── probe/          # 出口 IP 探测(sing-box 子进程)
│       ├── subscribe/      # 订阅三格式生成
│       ├── geo/            # IP 归属(可配 provider + 缓存)
│       ├── cryptox/        # AES-GCM 加密
│       ├── httpx/          # problem+json / 中间件
│       └── config/         # 配置加载
└── deploy/                 # Dockerfile / docker-compose / systemd
```

## License

MIT
