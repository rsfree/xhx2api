# 线上部署（1task.cn）

**已上线**：https://xhx2api.1task.cn（ai-prod = 210.121.44.245，`/opt/xhx2api`，
容器 `ghcr.io/rsfree/xhx2api:sha-<提交>`，端口 `127.0.0.1:39026`）。
运维手册见 `deploy/ai-prod/README.md`。

目标形态：`https://xhx2api.1task.cn` → nginx（TLS）→ `127.0.0.1:39026` 容器。

## 1. 配置

```bash
mkdir -p deploy/conf
cp config.example.json deploy/conf/config.json
```

改三处：

```jsonc
{
  "listen": "127.0.0.1:7867",              // 只让本机 nginx 访问
  "api_key": "sk-xhx-<自己生成>",           // 公网必须设！空 = 谁都能用你的账号
  "login": {
    "callback_base": "https://xhx2api.1task.cn"   // 浏览器授权的回跳地址
  }
}
```

`callback_base` 是**线上和本机唯一需要区别对待**的配置：

| 场景 | 取值 | 说明 |
|---|---|---|
| 本机自用 | 留空 | 自动推导 `http://127.0.0.1:<端口>` |
| 线上域名 | `https://xhx2api.1task.cn` | 授权完成后浏览器回跳到这个域名，网关收 code 换票 |

上游 authorize 页接收域名 redirect（2026-10-26 实测：页面原样透传，不回退
`office-raccoon://`）。**但即使它某天开始拒绝**，也不影响可用性：
用户把跳转后地址栏里的整条链接粘到面板「粘贴凭证」即可（`code` 就在里面）。

## 2. 起容器

```bash
mkdir -p auths data
docker compose -f deploy/docker-compose.yml up -d --build
docker compose -f deploy/docker-compose.yml logs -f
```

看到 `已启动：监听 127.0.0.1:7867` 即可（容器内 7867，ai-prod 宿主映射到 39026）。

> 挂载目录属主不对会报 `permission denied`（写 `auths/*.json.tmp` 失败）：
> `PUID=$(id -u) PGID=$(id -g) docker compose -f deploy/docker-compose.yml up -d`
> 或 `sudo chown -R 10001:10001 auths data deploy/conf`。

> 🔴 **配置必须目录挂载**（`./conf:/app/conf` + `-config /app/conf/config.json`）。
> 面板保存走「写 `config.json.tmp` → `rename` 覆盖」，而 `rename` 如果目标是
> 单文件 bind mount 的 mountpoint，内核直接返回 `EBUSY`，面板报
> `rename /app/config.json.tmp /app/config.json: device or resource busy` 且配置不变。
> 老部署（单文件挂载）升级时按 `deploy/ai-prod/README.md` 的「配置挂载迁移」操作。

## 3. nginx + 证书

生产（ai-prod）的完整部署资产在 **`deploy/ai-prod/`**，照那个目录走即可：

```
deploy/ai-prod/
├── docker-compose.yml          # 镜像部署（端口只绑 127.0.0.1:39026）
├── nginx/
│   ├── xhx2api.1task.cn.conf       # :80，只留 ACME 挑战 + 301
│   ├── xhx2api.1task.cn-ssl.conf   # :443，include 下面的 .inc
│   ├── xhx2api-proxy.inc           # 反代参数（SSE 不缓冲 + 长超时）
│   └── apply-https.sh              # 幂等：DNS 预检 → 签证书 → 投 :443 → 验证矩阵
└── README.md                   # 运维手册（升级/回滚/备份）
```

```bash
# 在目标机上（root）
cp /opt/xhx2api/nginx/xhx2api.1task.cn.conf /www/server/panel/vhost/nginx/
nginx -t && nginx -s reload          # 先让 :80 通，ACME 才有通道
bash /opt/xhx2api/nginx/apply-https.sh
```

证书走 **HTTP-01 webroot**（`/www/wwwroot/acme`），不是 DNS-01 —— 域名已指向本机，
HTTP-01 不需要阿里云 AK 参与，续期也少一个失败点。acme.sh 的 `reloadcmd` 已配 nginx reload。

🔴 `proxy_buffering off` 和 `proxy_read_timeout 3600s` 是**必须**的，两个都踩过：
- 开缓冲 ⇒ SSE 攒够才吐，客户端表现为「一直等、最后一次性出来」；
- 默认 60s 超时 ⇒ 非流式对话实测能到 137s、生图 4K 档 148s，全部被掐断，
  用户看到的是「网关偶尔卡死」，而不是超时错误。

## 4. 验收

```bash
# 只读项（不耗积分）
go run ./cmd/livecheck -base https://xhx2api.1task.cn -key <api_key> -skip-chat
# 全量（会消耗几次对话的积分）
go run ./cmd/livecheck -base https://xhx2api.1task.cn -key <api_key>
```

全绿就可以把客户端指过去了：

```bash
export ANTHROPIC_BASE_URL=https://xhx2api.1task.cn
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

## 5. 安全底线

- **`api_key` 必须设**：为空等于把账号借给全网。
- 面板与 `/v1/*` 同一把钥匙；面板支持 `?key=` 直达，分享链接时注意别外传。
- 建议在 nginx 层再加一道 IP 白名单或 `limit_req`（本仓未内置限流）。
- `auths/` 里是明文 refresh token：不要进备份/同步盘，不要提交到仓库。
