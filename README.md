# xhx2api — 小浣熊多账号网关

<p>
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI%20%7C%20Anthropic%20%7C%20Responses-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single%20Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="License" src="https://img.shields.io/badge/License-MIT-green?style=flat-square">
</p>

把商汤「小浣熊」（`xiaohuanxiong.com`）网页端 / 桌面端的登录态，包成本地一个
**协议兼容**的模型网关：账号池轮转、自动续期、每日签到、积分对账、Web 面板，
单二进制部署。

- **OpenAI SDK / curl** → `POST /v1/chat/completions`
- **Claude Code / Anthropic SDK** → `POST /v1/messages`（含 `count_tokens`）
- **Codex / 新版 Agent** → `POST /v1/responses`
- **文生图 / 图生图 / 多图（OpenAI Images）** → `POST /v1/images/generations`。
  不带 `image` 就是纯文生图；带 `image` 时网关自动转 multipart 打上游 `/images/edits`。
  `image` 三种写法都收：**单张字符串**、**字符串数组**、**`null`**（= 没有图）；
  每个元素可以是 **data URI** 或**裸 base64**（含 URL-safe / 缺 padding 的变体）。
  `POST /v1/images/edits` 同时收 multipart 与 JSON：multipart 下 `image` / `image[]`
  既可以是文件，也可以是 base64 文本字段（同名重复即数组）。

> 本项目与商汤科技**没有任何关系**，非官方、未获授权，仅用于**本人授权账号**在
> 本机 / 私有环境下的协议互操作。请遵守上游服务条款：**不要**用于对外中转、批量
> 注册小号、售卖额度或其他滥用行为。由此产生的一切后果由使用者自负。

> 仓库不含任何账号、token 或密钥。凭证只存在于你自己的 `auths/` 目录（0600）。

---

## 它解决什么问题

单个小浣熊账号有每日积分上限，而编码代理（Claude Code / Codex）一天能打光好几份。
本项目把多个账号合成一个端点，并按「谁便宜/谁还有额度/谁刚休息过」自动选号，
同时保证：

- **不误伤账号**：参数错误、上下文超限不会把好号打进 12 小时冷却；
- **不丢凭证**：上游每次续期都会轮换 `refresh_token`，新票立刻原子落盘；
- **不假装成功**：上游用 HTTP 200 + 首帧 `{"error":…}` 表达限流/欠费，网关必须先
  看一帧再决定「换号重试」还是「如实回错」；
- **不编造金额**：只记 token 与请求数，积分换算表上游没公开，网关不猜。

---

## 上游是什么形状

| 能力 | 上游端点 | 说明 |
|---|---|---|
| 对话 | `POST {llm}/chat/completions` | 标准 OpenAI 形状；SSE 是 `data:` 帧 |
| 生图 | `POST {llm}/images/gen` | OpenAI Images 形状：`{prompt, size, quality}` → `data[0].b64_json`（**网关补 url**）。实测 20-200s，**忽略 `model`**（只有一条通道），目录里不列图片模型。**收到 `image` 会静默忽略**（实测 image_tokens=0、出无关新图）——图生图只能走 edits |
| 图生图 | `POST {llm}/images/edits` | 上游只吃 `multipart/form-data`（直接传 JSON 回 415，网关会替你转）。`image` 可重复或写成 `image[]`，**多图可用**（两张图 input image_tokens=2048）。实测换风格保留构图、双图合成可用 |
| 模型目录 | `GET {llm}/model_catalog` | `data.categories[].models[]`。`visible:false` 只表示「不在官网选择器里」，**不代表不可调用**：实测 3 个 `raccoon-*` 全是 false，其中就有默认模型 |
| 续期 | `POST {auth}/refresh` | 回 `data.access_token` / `refresh_token`（轮换） |
| 用户资料 | `GET {auth}/user_info` | `data.id` / `name` |
| 积分 | `GET {points}/balance` | `available_points` + 每日/每月/奖励/充值四桶 |
| 流水 | `GET {points}/bills` | 逐笔发放与消费，带 `expire_at`，可按标题对账 |
| 桌面端领取 | `POST {origin}/api/web/desktop/v1/login/points/grant` | 幂等，领过回 `granted:false` |
| 手机端首登 | `POST {origin}/api/web/mobile/v1/login/points/grant` | 与桌面端**另一笔**额度 |
| 换码登录 | `POST {auth}/login_with_authorization_code` | `office-raccoon://` 回调里的 code |
| 扫码登录 | `POST {auth}/login_with_qrcode_code` | 轮询 `pending/logging/success/canceled`。**2026-10-26 实测：轮询端点还在（任意 code 都回 `pending`），但二维码落地页 `/login/mp` 已 404** —— 二维码内容改由上游生成，当前实现不可用，请用浏览器授权 |

默认基址：

```
llm    https://xiaohuanxiong.com/api/web/llm/v2
auth   https://xiaohuanxiong.com/api/web/auth/v1
points https://xiaohuanxiong.com/api/web/points/v1
origin https://xiaohuanxiong.com
```

出站带桌面客户端识别头（`x-client-name: @raccoon/office-desktop`、
`x-client-version: v1.1.0` 等）。官方升版本后可用 `upstream.client_version` 覆盖，
不必重新编译。

---

## 快速开始

### 二进制

```bash
git clone https://github.com/jushenzhidao/xhx2api && cd xhx2api
go build -o xhx2api ./cmd/server
./xhx2api -config ./config.json
```

首次运行若 `config.json` 不存在，会**自动生成**一份带随机 `api_key` 的推荐配置
（密钥打印在启动日志里）。

```bash
curl -s http://127.0.0.1:7867/healthz
curl -s -H 'Authorization: Bearer <api_key>' http://127.0.0.1:7867/v1/models
```

面板：`http://127.0.0.1:7867/panel/`（与 `/v1/*` 同一把钥匙）。

### Docker

```bash
mkdir -p conf auths data
cp config.example.json conf/config.json   # 也可以留空，首次启动自动生成带随机 api_key 的配置
docker compose up -d --build
docker compose logs -f
```

容器以 uid `10001` 运行；挂载目录属主不同时：

```bash
PUID=$(id -u) PGID=$(id -g) docker compose up -d
# 或
sudo chown -R 10001:10001 auths data conf
```

> 🔴 配置是 `./conf:/app/conf` **目录挂载** + `-config /app/conf/config.json`。
> 别改成 `./config.json:/app/config.json` 单文件挂载：面板保存用「写 `config.json.tmp`
> → `rename` 覆盖」，`rename` 的目标是 bind mount 的 mountpoint 时内核返回 `EBUSY`，
> 面板会报 `device or resource busy` 且配置存不进去。

线上部署（Docker + nginx + 域名）见 [`deploy/README.md`](deploy/README.md)，里面含
SSE 必须的 nginx 配置（`proxy_buffering off`）与回调基址设置。

---

## 添加账号

面板「添加账号」提供四条路径，按部署环境选：

| 方式 | 适用场景 | 说明 |
|---|---|---|
| **客户端目录** | 装了官方桌面端并登录过 | 扫本机 `office-raccoon/agent/config/auth.json`，**只读**、一键导入（见下） |
| **手机号** | 手里有手机 | 手机号 + 短信验证码，走官方 `/send_sms` + `/login_with_sms`（见下） |
| **浏览器授权** | 浏览器能访问 `127.0.0.1:<网关端口>` 或你的域名 | 走官方 `/code/authorize` 页，回调把 code 送回网关 |
| **粘贴凭证** | 以上都不通 | 粘账号 JSON / 裸 `access_token` / `office-raccoon://auth/callback?...` 整条链接 |
| ~~微信扫码~~ | **当前不可用** | 上游 `/login/mp` 已 404（2026-10-26 实测，桌面端也用它，等于官方这条路也断了）；面板保留入口并提示 |

### 客户端目录（最省事）

官方桌面端把票明文写在：

```
macOS    ~/Library/Application Support/office-raccoon/agent/config/auth.json
Windows  %APPDATA%\office-raccoon\agent\config\auth.json
Linux    $XDG_CONFIG_HOME/office-raccoon/agent/config/auth.json
```

面板「添加账号 → 客户端目录 → 扫描本机」会列出来，勾选即导入。扫描是**只读**的；
导入前会拿票打一次 `user_info`（幂等 GET）：既验票是否还活着，也把 IAM 主体
（`pu-…`）换成 office 域数字 uid（不换的话同一个号会在池子里出现两行）。

⚠️ 客户端那份票是 **IAM 票**（`iss=xiaohuanxiong.com/iam`，30 天有效），**不带
`refresh_token`**——官方客户端自己也不续期，到期就重新登录。所以这条路适合
「快速把已经登录过的机器接进来」，长期号还是走浏览器授权/手机号（能续期）。
覆盖导入同一个号时会自动继承原有的 `refresh_token` 与余额/签到快照，不会降级。

额外路径（服务器/NAS 挂载、自定义安装位置）在 `config.json` 里加：

```jsonc
"import": { "client_dirs": ["/mnt/nas/office-raccoon"] }
```

目录会自动展开成 `agent/config/auth.json`、`config/auth.json`、`auth.json` 三个候选。

### 手机号验证码

「添加账号 → 手机号」：填手机号 → 点「发送验证码」（弹出阿里云人机验证）→ 填 6 位码 → 登录。

上游口径是从官方桌面端 asar 里解出来的（`accountRequests.js`），两个细节值得记：

- **手机号必须加密**：明文直接 `params_encryted_error`。算法是
  `base64(iv ‖ AES-128-CFB(phone, key="senseraccoon2023"))`，密钥来自网页登录页内联
  配置的 `config.publicKey`（名字骗人，其实是 16 字节对称密钥）。
  实现在 `upstream.EncryptCredential`，回归测试 `TestEncryptCredentialRoundTrip`。
- **人机验证绕不开**：`send_sms` 的 `captcha_param` 是阿里云验证码
  （`scene=1pkmy0x3` / `prefix=hk1r5l` / `region=cn`）的 `captchaVerifyParam`。
  实测假票返回 `100006 captcha_verify_error`。所以面板要真跑一次控件——
  为此 CSP 放行了 `*.alicdn.com`（验证码 SDK 从 o/g/x 三个 CDN 域名拉脚本）。

⚠️ **两条登录通道的验证码不通用**：网页/桌面端各有一套（IAM `/sms/send` +
`/auth/login/sms`，与 `/api/web/auth/v1/send_sms` + `login_with_sms`）。
**发码和验码必须是同一条**，混用会得到「验证码不正确」，即使填对了。
网关按「有没有填邀请人」自动选通道：填了走桌面端（只有它认 `inviter_user_id`），
不填走 IAM。

注册成功即入池。实测一个新号的起始额度是 **6000 分**：
`新人注册礼包 3000`（服务端注册时发）+ `桌面端登录奖励 3000`（网关签到时会去领）。

面板域名万一不在上游验证码白名单，控件会 `onError`，页面会自动展开
「手动填票据」：在网页版 `xiaohuanxiong.com/login` 点一次发送验证码，
F12 抄下 `send_sms` 请求体里的 `captcha_param` 粘进来即可。

**线上部署时**，浏览器授权要能回跳到你的域名：在 `config.json` 里设
`login.callback_base = "https://xhx2api.1task.cn"`（实测上游接受域名 redirect）。
即便回跳失败，把地址栏里那条 `.../auth/callback?code=…` 整条粘进「粘贴凭证」也能完成
——上面三种方式最终都归结为「用一次性 code 换票」。

账号落盘在 `auths/<uid>.json`（0600，原子写）。手动导入也支持：

```bash
cat > auths/4465567.json <<'EOF'
{
  "uid": "4465567",
  "nickname": "小浣熊-1",
  "access_token": "eyJ…",
  "refresh_token": "eyJ…",
  "expires_at": 1792369369000
}
EOF
```

`refresh_token` 强烈建议一起存：只有 access_token 的话，过期后无法自动续期。

---

## 接入客户端

**OpenAI SDK**

```python
from openai import OpenAI
client = OpenAI(base_url="http://127.0.0.1:7867/v1", api_key="<api_key>")
print(client.chat.completions.create(
    model="raccoon-8c4485",
    messages=[{"role": "user", "content": "你好"}],
).choices[0].message.content)
```

**Claude Code**

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:7867
export ANTHROPIC_AUTH_TOKEN=<api_key>
```

**Codex**

```toml
model_provider = "xhx"
model = "raccoon-chat-ml-5-5"

[model_providers.xhx]
name = "小浣熊"
base_url = "http://127.0.0.1:7867/v1"
env_key = "XHX_API_KEY"
wire_api = "responses"
```

**模型名**：`/v1/models` 来自上游目录。客户端写死的 `gpt-4o` / `claude-*` / `o3`
这类名字会经 `aliases` 表映射到上游 id；目录非空时未知模型直接 400，不会白打一次
上游。

---

## 配置

`config.json` 支持 JSONC（`//`、`/* */` 注释与尾逗号），**未知键名会报错**
（写错 `cooldwon` 静默忽略会让人排查半天）。

| 键 | 默认 | 说明 |
|---|---|---|
| `listen` | `0.0.0.0:7867` | 监听地址（改需重启） |
| `api_key` | 随机生成 | `/v1/*` 与面板的密钥；空 = 不鉴权（仅本机） |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 凭证目录 / 池状态 |
| `default_model` | `raccoon-8c4485` | 请求未给 model 时使用 |
| `aliases` | `gpt-4o → raccoon-chat-ml-5-5` 等 | 通用名映射 |
| `upstream.*` | 见上表 | 四个基址 + 三档超时 + 客户端版本 |
| `cooldown.plan_credit` | `12h` | 额度不足冷却（`0` = 到次日 04:00） |
| `cooldown.soft_rate` / `soft_rate_max` | `60s` / `2h` | 限流软冷却基数与封顶 |
| `pool.strategy` | `round_robin` | `round_robin` / `priority` / `weighted_random` / `least_inflight` |
| `pool.max_in_flight` | `5` | 单号在途软上限。上游硬上限就是 5（2026-11-01 实测：第 6 个起 0.1s 内 429，按账号算且跨模型共享）；超发不会多挤一个，只会把号打进上游的频率惩罚 |
| `pool.queue_wait` | `2s` | 全部账号在途满时的最长等待；超时软回退（多排一个）。`0` = 不等待 |
| `pool.breaker_*` / `degrade_*` | `3 次 / 10m` | 熔断与降权阈值 |
| `schedule.checkin_hours` | `[9]` | 每日签到时点（东八区） |
| `schedule.keepalive_hours` | `[3]` | token 保活时点 |
| `schedule.balance_refresh_minutes` | `5` | 余额刷新周期（0 = 关闭） |
| `schedule.include_disabled_in_tasks` | `false` | 已禁用账号是否也签到/续期 |
| `session_sticky.*` | `true` / `30m` | 会话粘性：客户端给了 `X-Session-Id` / `prompt_cache_key` 时同一对话尽量同号 |
| `session_sticky.prefix_affinity` | `false` | 没有会话 id 时也按消息前缀粘性。打开后同一 system 的**全部**请求会钉在一个号上（多号池退化），只在需要压低风控特征时开 |
| `prompt.mode` | `passthrough` | `custom` 时用 `prompt.file` 替换 system |
| `images.enabled` | `true` | 是否开放 `/v1/images/generations`（改了要重启） |
| `images.model` | 空 | 指定上游图片模型；空 = 上游默认组 `raccoon-image-gen`。**实测上游忽略这个字段** |
| `images.aliases` | `gpt-image-1/1-mini/2/2.5`、`dall-e-2/3` | 额外接受并暴露的图片模型名（热改生效）。上游只有一条生图通道，别名不改实际模型，纯粹方便客户端选中；`[]` = 全关 |
| `images.timeout_seconds` | `600` | 单张图超时。实测 1024 档 49-143s、4K 档 150s，但超限尺寸压缩后单次能到 **267s**（300s 只剩 33 秒余量），故 600 |
| `images.response_format` | `url` | 客户端**没传** `response_format`（或传 `null` / `None`）时的默认形态：`url` 只回链接、`b64_json` 只回 base64、`both` 两个都给。改了要重启 |
| `images.return_url` | `true` | 是否提供可访问的 `url`（图落盘或传对象存储，`/v1/images/<id>`）。显式 `false` 后只能回 base64，点名要 url 会 400。改了要重启 |
| `images.max_total_mb` | `2048` | 落盘目录总量上限；`0` = 不限制。超限按 mtime 删最旧的到 90%（旧链接会 404）。改了要重启 |
| `images.public_base` | 空 | 写进 url 的对外基址（如 `https://xhx2api.1task.cn`）。反代后面不填会生成内网地址 |
| `images.dir` | 空 | 落盘目录（空 = `<state 同目录>/images`）；`return_url=false` 且没接 COS 时不落盘 |
| `images.cos.*` | 关闭 | 传腾讯云 COS，url 用 COS 地址。字段：`secret_id` / `secret_key` / `bucket`（含 APPID）/ `region` / `prefix` / `base_url`（CDN 域名）/ `public_read`（带 `x-cos-acl: public-read`）/ `signed_ttl_seconds`（>0 时给临时签名链接）。开了自动启用 `return_url`；**上传失败回落本地盘**，不影响出图 |
| `models.hidden` | `[]` | 黑名单：不列出 + 点名拒绝 |
| `models.include_upstream_hidden` | `true` | `/v1/models` 也列出上游 `visible=false` 的模型。关掉则只列它官网展示的（需重启） |
| `server.min_output_tokens` | `2048` | 出站 `max_tokens` 下限。思考 token 算在额度里，客户端给 16/512 时正文会回空。**负数 = 关闭抬升**（0 会被当默认值 2048） |
| `login.inviter_user_id` | 空 | 注册时的邀请归因 uid（纯数字）。**实测无效**：上游不落库（`registration_attribution` 仍为空）、双方都不加分 |
| `import.client_dirs` | `[]` | 额外扫描的客户端凭证路径（文件或目录）。默认按平台查标准位置；服务器部署留空即可 |
| `login.callback_base` | 空 = 自动推导 | 浏览器授权完成后的回跳地址。本机留空（自动 `http://127.0.0.1:<端口>`）；线上填 `https://xhx2api.1task.cn` |
| `logging.request_*` | 归档 7 天 / 100MiB | 请求元数据 JSONL（不含正文） |

环境变量覆盖：`XHX_API_KEY`、`XHX_LISTEN`、`XHX_AUTH_DIR`、`XHX_STATE_FILE`、
`XHX_DEFAULT_MODEL`、`XHX_UPSTREAM_*`、`XHX_MODELS_HIDDEN`（逗号分隔）。

面板「配置」页可在线改并**热生效**：`api_key`、`cooldown.*`、`pool.*`、
`schedule.*`、`models.hidden`、`prompt.*`、`logging.request_client_info`。
`listen` / `auth_dir` / `upstream.*` / `session_sticky.ttl` 改完会提示需重启。

---

## 错误口径（这套网关的核心）

上游的失败有好几种，处理错一种就会误伤账号：

| 上游表现 | 分类 | 网关动作 | 账号惩罚 |
|---|---|---|---|
| 401 / 403 | `session_dead` | **先续期**，续上就重试；续不上才换号 | 连续 3 次才自动禁用 |
| 429 | `soft_rate` | 换号 | 软冷却 60s 起，指数退避封顶 2h |
| 429/400 且文案明确「积分/额度不足」 | `plan_limit` | 换号 | 冷却 12h（或到次日 04:00） |
| 5xx | `server` | 换号 | 连续 3 次熔断 10m，逐次翻倍 |
| 502/504 网关超时 | `gateway` | 换号 | 只回避 15s，不罚号 |
| `serviceAvailable:false` 排队 | `busy` | 换号 | 只回避 45s，不罚号 |
| 其余 4xx（模型名/参数/上下文） | `client` | **不换号**，如实回错 | 不罚号 |
| 5xx 但文案是参数错误（`Unexpected keyword argument` 等） | `client` | 不换号，如实回错 | 不罚号（实测：上游对未知字段严格校验，会以 500 返回） |
| 200 但首帧是 `{"error":…}` | 按内容分类 | 没写字节前换号 | 同上 |
| 200 但流是空的 | `server` | 非流式可换号；流式截流 | 计一次失败 |

两条刻意收窄的判据（都有实测教训）：

- **`insufficient` 单独出现不算欠费**：`context window is insufficient` 也会命中，
  曾把好号打进 12 小时冷却。必须同时出现「积分/额度/余额/quota」类词。
- **`token` 单独出现不算登录失效**：`max_tokens exceeded` 到处都有，曾让账号被连续
  记错直至自动禁用。

---

## 容量实测（2026-10-26）

单账号、限免模型（x0，不扣分）、`max_tokens=32`，直连上游绕过网关排队：

| 并发 | 成功 | p50 | p90 | 429 |
|---|---|---|---|---|
| 1 | 1/1 | 80.8s | — | 0 |
| 2 | 2/2 | 13.3s | — | 0 |
| 4 | 4/4 | 36.6s | 44.6s | 0 |
| 8 | 8/8 | 20.4s | 41.7s | 0 |
| 10 | 10/10 | 16.8s | 63.2s | 0 |
| 12 | 10/12 | 17.7s | 41.1s | **2** |
| 16 | 12/16 | 25.3s | 49.6s | **4** |

- 拐点在 **5 并发**：第 6 个在途请求起立刻 429（`user_model_rate_limited` 与
  `200103 limiter_request_error` 两种文案混用），0.1s 内返回、**不排队**。
- **限流按账号算、跨模型共享**：同号两个模型各 5 并发同时打 → 5/10 成功。
- 两条独立的闸：①在途 5 个（秒级恢复）；②累计请求频率（几十发/分钟就打满，
  之后**所有模型**一起 429，实测 ≥30 分钟才解）。所以「慢一点重试」救不了第 ② 种。
- 结论：`max_in_flight` 默认 **5**；持续高压时网关会靠换号吸收，但单号吞吐仍有上限，
  要扛更高 QPS 只能加号。

复跑：`go run ./cmd/loadtest -uid <uid> -c 8 -n 8`

## 积分怎么来（2026-10-26 实测）

上游是**发放制，不是任务制**——没有「做任务换积分」，也没有邀请返利。全部来源：

| 来源 | 金额 | 触发 | 能否自动化 |
|---|---|---|---|
| 新人注册礼包 | 3000 | 注册即发 | 不用管 |
| 桌面端登录奖励 | 3000 | 一次性，`desktop/v1/login/points/grant` | ✅ 入池即领（幂等） |
| 移动端登录奖励 | 3000 | 一次性，`mobile/v1/login/points/grant` | ✅ 入池即领（幂等） |
| 每日积分 | 300/日 | 每天 00:00（东八区）发放，当天登录把有效期延到 7 天 | 靠保活 |
| 会员/加油包/兑换码 | 付费 | 商城 | 不涉及 |

所以**一个新号进池立刻值 9000 分**（注册 3000 + 桌面端 3000 + 移动端 3000）。
网关在 `admitQuiet` 后会自动领后两笔（`panel.claimWelcome`），不用等到签到时段。

### 扣费怎么算（实测）

上游目录只给**倍率**，不给单价。实测（同一账号、同一 prompt、读积分流水）：

| 模型 | 倍率 | tokens | 扣分 |
|---|---|---|---|
| `raccoon-8c4485` | x1 | 1,982 | **3** |
| `raccoon-chat-ml-5-5` | 目录未公布 | 1,990 | **3** |
| `sn-glm-5-3-flash` | x0.2 | 1,821 | 2 |
| 任意模型（极短请求） | — | 16 / 364 | 2 / 2 |

推出两条：

1. **每请求最低 2 分**（16 token 和 364 token 都是 2 分）；
2. **x1 约 0.5 分/千 token**，按倍率线性缩放（x0.2 → 约 0.1）。

所以「一次几毛钱」这种直觉在这里不成立：**短请求的固定成本占大头**，批量合并
一次长对话比拆成十次短问答便宜得多。模型页每张卡片上的「≈ 分/千token」就是
按这个基准 × 目录倍率推的估值，实际以积分流水为准。

顺带确认：默认别名指向的 `raccoon-chat-ml-5-5` 虽然不在目录里，但**计费与 x1 同档**
（1,990 token 扣 3 分，和 `raccoon-8c4485` 一致），可以放心用 `gpt-4o` 这个名字调。

面板「积分」页把每个号的来源与到账情况列出来，并可一键补领；
另有「近 7 天消耗 / 预计可用天数」两列（按 100 条流水算日均，流水不够时会偏低）。
注意「每日 300」当天注册的号可能不到账（实测第二个号注册当天没有），
次日 00:00 起才有——不是漏发。

## 模型列表怎么看

面板「模型」页是卡片，一屏回答四个问题：**叫什么、贵不贵、多大、能用什么名字调**。

- 标题是**模型 ID**（点一下复制），第二行是上游给的一句「适合做什么」——上游的
  `name` 字段其实是描述，不是名字，早期把它当标题显示是反的；
- 右上角是倍率；限免的显示绿色「免费」并把原倍率划掉，下面一行给出截止时间；
- 标签区分 `默认` / `思考` / `视觉` / `上游未列出` / `已拉黑`；
- 「别名」一行列出所有指向它的通用名（`gpt-4o`、`o3`…），点一下复制，搜索框也认别名；
- 末尾虚线卡片是**目录外模型**（别名指向、上游目录里没条目，但实测可调用）。

按倍率从低到高排序，同价按 ID 排；顶部有「只看免费」。

### 十个模型实测（2026-10-26）

同一个 prompt（「只回复两个字：收到」）、串行、直连上游。**耗时波动很大**
（同一模型两次采样能差 4 倍，如 `raccoon-8c4485` 实测 9.8s 与 37.4s），下表是一次采样：

| 模型 | 倍率 | 首字 | 总耗时 | tokens | 思考 |
|---|---|---|---|---|---|
| `sn-glm-5-3` | x0.75 | 5.6s | 9.8s | 122 | 102 字 |
| `sn-deepseek-v4-1-flash` | x0.25 | 7.2s | 13.0s | 58 | 21 字 |
| `sn-glm-5-3-flash` | x0.2 | 9.7s | 15.5s | 81 | 61 字 |
| `sn-deepseek-v4-1-flash-1017` | x0（限免） | 10.2s | 14.5s | 51 | 14 字 |
| `sn-sensenova-6-8-flash` | x0（限免） | 11.8s | 18.2s | 268 | 214 字 |
| `sn-sensenova-6-8-flash-lite` | x0（限免） | 14.0s | 17.8s | 315 | 224 字 |
| `raccoon-405a1c` | x1 | 16.3s | 21.6s | 16 | — |
| `sn-kimi-k3` | x1 | 17.4s | 28.6s | 217 | 113 字 |
| `raccoon-19b265` | x1 | 26.7s | 32.3s | 16 | — |
| `raccoon-8c4485` | x1 | 14.2s | 17.2s | 16 | — |

两条结论：

- **`sn-*` 全是思考型**：一句「收到」也要烧 21-224 个思考 token，`max_tokens` 小会
  直接回空；`raccoon-*` 不思考，16 token 就结束。
- **思考量波动极大**：同一个 `sn-sensenova-6-8-flash`、同一句话，实测思考长度从
  90 token 到 4000+ 字都可能。所以网关的下限只是「降低概率」，真正的兜底是
  **检测到空正文就自动加额重试一次**（见下）。
- **首字最快的是 `sn-glm-5-3`（5.6s）**，最慢的是 `raccoon-19b265`（26.7s）。
  编码代理这种「先等首字」的场景，`sn-glm-5-3` 体验最好，代价是 0.75 倍率 + 思考开销。

复跑：`go run ./cmd/livecheck -base http://127.0.0.1:7867 -key <key> -all-models`
（每个付费模型约 2 分，限免模型 0）

`/v1/models` 与页面口径一致：默认连上游 `visible=false` 的模型一起列（并标
`unlisted: true`）。按上游的展示标记过滤会让客户端看不到网关的默认模型——
它的默认值是 `raccoon-8c4485`，而上游把这个模型标成 `visible:false`。

## 生图（2026-10-26 实测）

上游有独立生图端点，返回 **OpenAI Images 形状**，网关做的是「选号 + 透传 + 计量」：

```bash
# 默认就回 url（网关落盘 + /v1/images/<id>）
curl -s http://127.0.0.1:7867/v1/images/generations \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"prompt":"一只戴帽子的橘猫，扁平插画风格","size":"1024x1024","quality":"high"}' \
  | jq -r '.data[0].url'

# 要 base64 就显式说（此时不落盘）
curl -s ... -d '{"prompt":"...","response_format":"b64_json"}' \
  | jq -r '.data[0].b64_json' | base64 -d > cat.png
```

实测数据：

| 参数 | 结果 |
|---|---|
| 必填 | 只有 `prompt`（空值上游直接 400） |
| `model` | **完全被忽略**：传 `gpt-image-2` / `gpt-image-2.5` / 甚至捏造的 `definitely-not-an-image-model-xyz` 都 200，`image_tokens` 一模一样 |
| 实际模型 | **gpt-image-2**（OpenAI 系），与 Seedream / 商汤 U1 Pro 无关。产物 PNG 里的 C2PA 签名可自证：一部分盖 `Azure OpenAI ImageGen`（微软 invismark），一部分盖 `gpt-image-2` + `OpenAI OpCo, LLC`；少数不带签名的，其单 IDAT 编码与直连渠道同构，属于同源剥签名 |
| `size` | **生效**，上限是「最长边 3840 + 总像素 8,294,400」：`1024x1024`→196 tokens、`1536x1024`→158、`1792x1024`→139、`3840x2160`(4K)→371。超限的两种失败模式见下 |
| `quality` | **生效**，省略时默认 **`low`**：4K 档 `high` 把 `image_tokens` 从 371 拉到 13342（36 倍），1024 档从 229 到 7050 |
| `size` 省略 | 上游**自选比例**：早期实测 `1254x1254`（229 tokens），2026-10-27 复测为 `1402x1122`（186 tokens）——不要当成固定值 |
| `size=auto` | 上游认这个值，但给的是 **`1024x1024`**（196 tokens），和不传**不一样** |
| 返回 | 网关默认给 `data[0].url`（图落盘/传 COS，`/v1/images/<id>`）；显式 `response_format=b64_json` 时给 `b64_json`。都带 `usage.total_tokens`。上游本身只回 `b64_json`，`url` 是网关补的 |
| 耗时 | 20-200s（比对话慢一个数量级）：1024 档 20-80s，4K 档 136-148s，`quality` 对耗时影响很小 |
| 计费 | 实测**不扣积分**：直连上游（196 image_tokens）与经网关（213 tok）各测一次，按 `bill_id` 求差集，两个号余额与流水**均无变化**。注意别把「客户端 agent 画图」的扣费算到这里——那是 agent 的 LLM 轮次在花钱（`任务消耗 · <会话标题>`）。可能限免或未接计费，别当承诺 |

`model` 那一行不是笔误：这条通道只有 LiteLLM 的 `raccoon-image-gen` 一个组，
**没有真正的模型选择能力**。`/images/gen` 的响应体里连 `model` 字段都不回。

### 尺寸上限：最长边 3840 + 总像素 8,294,400

「4K」不是指 4096——上游有**两条**约束，失败方式还完全不同：

1. **最长边 ≤ 3840**（LiteLLM 硬校验）。`4096x4096` 会回
   `Invalid size '4096x4096'. The longest edge must be less than or equal to 3840.`
   **而且是等了 106 秒之后才报**——上游先跑了一遍生成才校验。
2. **总像素 ≤ 8,294,400**（= 3840×2160，生成侧预算）。`3840x3840` 边长完全合法，
   但**不报错**，直接给一张 2880×2880——而 `2880²` 正好等于 `3840×2160`。
   这种静默降级比报错更难发现：客户端以为拿到了 4K。

所以真正能拿到的最大尺寸是 `3840x2160`（横）或 `2160x3840`（竖），方形最多 `2880x2880`。
网关在转发前用 `upstream.ClampImageSize` 等比压到两条约束内，响应头
`X-XHX-Image-Size` 给出实际提交的尺寸：

| 请求 | 实际提交 | 说明 |
|---|---|---|
| `3840x2160` | 原样 | 正好卡在两条上限上 |
| `4096x4096` | `2880x2880` | 边长和像素都超 |
| `3840x3840` | `2880x2880` | 边长合法、像素超（上游本来也会静默降级到同一个值） |
| `5120x2880` | `3840x2160` | 保持 16:9 |
| `7680x4320` | `3840x2160` | 8K 压到 4K |
| `4096x2048` | `3840x1920` | 只超边长、像素没超 |

边长与压缩后的尺寸都向下对齐到 8 的倍数（实测上游接受的 1024/1536/1792/2160/2880/3840 都是 8 的倍数）。

### 图片模型别名

既然上游忽略 `model`，网关把常见名字做成了**别名**（默认全开）：

```
gpt-image-1  gpt-image-1-mini  gpt-image-2  gpt-image-2.5  dall-e-2  dall-e-3
```

它们会出现在 `/v1/models`（`capabilities.image=true` + `alias_of=raccoon-image-gen`），
所以 Cherry Studio / OpenWebUI / LobeChat 这类把 `gpt-image-1` 硬编码成默认图片模型
的客户端，直接选中就能出图，不用改配置。拿别名调对话会 400 并指向生图端点。

要关掉：`images.aliases: []`（显式空数组；省略该键 = 用默认表）。这个字段**热改生效**。

三个刻意的设计：

- **不转发客户端的 `model`**：改名和透传都没有意义（上游根本不读），而且传
  `dall-e-3` 这类名字曾让 LiteLLM 报 `No fallback model group found`。想指定具体
  图片模型用 `images.model`。
- **默认返回 url，而不是 base64**：上游只回 base64（一张 4K 图 8MB+），所以网关
  自己把图落盘/传对象存储并提供 `/v1/images/<id>`。**不传 `response_format`
  （或传 `null` / `None`）就回 `url`**，且不再附 `b64_json`；要 base64 就显式传
  `response_format=b64_json`（此时完全不落盘）；想两者都要把
  `images.response_format` 设成 `both`。落盘失败/没有存储时自动回落 base64，
  并在响应头给 `X-XHX-Image-URL: unavailable`——绝不返回 `url: null`。
- **超时不换号**：等满一个窗口已经 100 秒，再换号继续等没有意义；5xx / 限流才换。

生图会抢账号容量：实测「生图 + `raccoon-8c4485` 对话」并发时，对话侧 180s 超时。
所以网关给生图单独的超时（`images.timeout_seconds`），不和对话共用。

### 默认值与并发（2026-10-26 实测）

`size` / `quality` 都不传时的真实默认值：

| 请求 | 实际尺寸 | image_tokens | 说明 |
|---|---|---|---|
| 两者都不传 | `1254x1254` | 229 | 上游自己的默认，比 1024 多 17% 像素 |
| `size=auto` | `1024x1024` | 196 | 认这个值，但语义是「标准档」 |
| `size=1024x1024` | `1024x1024` | 196 | |

`quality` 无论哪种情况都回 `low`。

并发（同一账号 7772869、`1024x1024`）：

| 并发数 | 结果 | 最慢 | 总墙钟 |
|---|---|---|---|
| 4 | 4/4 成功 | 167.2s | 167.2s |
| 8 | 8/8 成功 | 100.0s | 100.1s |
| 12 | **10 成功 + 2 个 429** | 152.9s | 152.9s |

- **单账号上限 10 个**，和对话共用同一份配额（对话也是 12 起 429）。
- 429 的正文固定是 `{"code":200103,"message":"limiter_request_error","details":"request exceed max time, retry after:0s"}`，
  **0.1 秒立即返回**，不是封禁。
- **等 1 秒重试 100% 成功**：12 并发首发 10 成功，被拒的 2 个重试后 12/12。所以网关对生图的
  429 是「原地等 2 秒重试」而不是换号或罚号（`imageRateRetryDelay`）。
- 8 并发反而比 4 并发快（100s vs 167s）：上游是多节点并行，单次慢不等于并发差。
- 生图 8 + 对话 4 混跑（同账号、对话延后 1 秒）**12 个全过**——说明 429 看的是瞬时峰值，
  不是严格的在途计数。

生图与对话**共用**同一个 5 并发配额，所以 `pool.max_in_flight` 必须按 5 算，
不能按模型分开给：一边跑满生图，对话就会 429。

### 线上全形态验收（2026-10-26，https://xhx2api.1task.cn）

9 个出图形态 + 4 个负向校验，串行跑完（每张都解 base64 验 PNG 魔数与真实像素）：

| 形态 | HTTP | 耗时 | 回显尺寸 | 实际像素 | 体积 | tokens |
|---|---|---|---|---|---|---|
| 文生图 1024 | 200 | 103.8s | 1024x1024 | 1024×1024 | 802KB | 220 |
| 文生图 + `model=gpt-image-2` | 200 | 142.9s | 1024x1024 | 1024×1024 | 167KB | 207 |
| 文生图 `quality=high` | 200 | 75.0s | 1024x1024 | 1024×1024 | 1094KB | **7036** |
| 文生图 4K `3840x2160` | 200 | 149.6s | 3840x2160 | 3840×2160 | **4988KB** | 385 |
| 文生图超限 `4096x4096` | 200 | **267.2s** | 2880x2880 | 2880×2880 | 4019KB | 671 |
| 图生图 multipart | 200 | 176.7s | 1024x1024 | 1024×1024 | 702KB | 1241 |
| 图生图 multipart + mask | 200 | 62.6s | 1024x1024 | 1024×1024 | 278KB | 1237 |
| 图生图 JSON data URI | 200 | 49.1s | 1024x1024 | 1024×1024 | 283KB | 1232 |
| 图生图 JSON 多图（2 张） | 200 | 234.0s | 1024x1024 | 1024×1024 | 699KB | 2258 |

负向校验（全部 400 且 `error.code` 正确）：空 prompt、`mask` 不带 `image`、
`image` 传外链 URL、`response_format=png`（非法值）。

> 这一轮跑的时候 `response_format=url` 还是「400 说清楚」；现在默认就回 url，
> 那条用例已改成「不传 → 拿到 url」的正向断言（`TestImageDefaultsToURL`）。

三个值得注意的点：

- **图生图是真的用了输入图**：给「白底 + 蓝圆 + 红方块」并要求「方块改金色」，
  返回的图蓝圆与构图完全保留、方块变金色 —— 不是重新生成一张无关的图。
- **输入图会显著抬高 tokens**：单图文生图 220，单图图生图 1232，双图 2258。
- **4K 的响应有 5MB**（base64 后的 PNG）。客户端要有对应的内存与超时余量，
  面板/网关侧的 `imageBodyLimit` 是 64MiB，够用。

`/v1/models` 里会多出合成条目 `raccoon-image-gen`（`capabilities.image=true`）以及
它的别名，方便客户端发现；拿它们调对话会返回 400 并指向生图端点。

## 真号联调实测（2026-10-26）

用 `cmd/livecheck` 对一个真实账号（uid 7772869）跑完整链路，8 项全过：

```
✓ 健康检查            200 OK
✓ 模型目录            10 个真实模型（raccoon-8c4485 / sn-sensenova-6-8-flash / sn-glm-5-3 …）
✓ 账号池              6013 分，签到状态正确
✓ 对话（流式）        29 帧，回复「收到」
✓ 对话（非流式）      回复「收到」
✓ Anthropic 入口      stop_reason=end_turn
✓ Responses 入口      status=completed
✓ 用量记账            累计 16 次请求
```

复跑：

```bash
go run ./cmd/livecheck -base http://127.0.0.1:7867 -key <api_key>
# 只跑不消耗积分的检查
go run ./cmd/livecheck -base ... -key ... -skip-chat
# 额外验证生图（慢：20-200s，当前不扣分）
go run ./cmd/livecheck -base ... -key ... -skip-chat -image
# 逐个模型巡检（上新模型后跑一次）
go run ./cmd/livecheck -base ... -key ... -all-models
```

**实测出来的上游事实**（与参考实现的差异，改代码前请先看）：

| 事实 | 数据 |
|---|---|
| 模型目录 | 10 条，`context_window` 最高 **1,000,000**、`max_tokens` 100,000 |
| 限免模型 | `sn-sensenova-6-8-flash` / `-lite` / `sn-deepseek-v4-1-flash-1017` 为 `x0`（限免至 10-31），实测**不扣积分** |
| 主力倍率 | `raccoon-8c4485` x1、`sn-glm-5-3` x0.75、`sn-deepseek-v4-1-flash` x0.25、`sn-glm-5-3-flash` x0.2 |
| 积分与请求对账 | 一次 16 token 的对话扣 **2 分**；`X-RACCOON-Title` 会出现在流水 `event_name`（「任务消耗 · <title>」） |
| 扫码登录 | `/login/mp` **404**（详见上表）；默认 code 形状是 `13 位毫秒时间戳 + 32 位 hex` |
| 手机号加密 | `base64(iv‖AES-128-CFB(phone, "senseraccoon2023"))`；明文 → `params_encryted_error` |
| 人机验证 | 阿里云验证码 `scene=1pkmy0x3` `prefix=hk1r5l` `region=cn`；假票 → `100006` |
| 桌面端凭证 | `office-raccoon/agent/config/auth.json`，只有 `access_token`（IAM 票，30 天，**无 refresh**） |
| 严格参数校验 | `raccoon-*` 对未知字段回 500；`sn-*` 会忽略 |
| 上游 `visible:false` | 3 个 `raccoon-*` 全被标 false，但都可调用（其中一个就是默认模型） |
| 目录外但可调用 | `raccoon-chat-ml-5-5` 不在目录里，实测 HTTP 200；默认别名 `gpt-4o` / `claude-*` 都指向它 |

## 面板

| 页 | 内容 |
|---|---|
| 账号 | 池总览、每号积分/签到/冷却原因/在途/成功率/延迟；单号签到、刷新余额、暂停、禁用、解冻、移除（凭证备份到 `auths/removed/`） |
| 模型 | 上游目录（上下文、上限、倍率、限免标记），一键从上游刷新 |
| 请求 | 请求量/成功率/P50/P95/RPM，最近 100 条明细（含换号次数与首字节耗时） |
| 用量 | 按账号、按日的 token 与请求数（**不含金额**） |
| 日志 | 运行日志环形缓冲（最近 300 行，自动刷新） |
| 配置 | 直接编辑 `config.json`，保存后热应用 |

---

## 设计取舍

- **不注入提示词**（默认 `passthrough`）：编码代理对 system 结构敏感，替换会让工具
  调用退化。需要时可以显式设 `prompt.mode=custom`。
- **不做会话存储**：三个协议都无状态，`previous_response_id` / `store` 由客户端维护。
- **不换算金额**：只记 token。积分单价没有可信公开表，编一个数字出来只会误导对账。
- **签到 ≠ 调接口发分**：每日积分是服务端 00:00 自动发放的，`grant` 是登录弹窗式的
  幂等领取。所以「签到成功」以 `bills` 流水里当天有 `daily_grant` 为准；当天注册的
  新号与从未获得过每日积分的号会被单独识别成「待发放 / 无资格」，不谎报失败。
- **归档不含正文**：请求体里有用户的代码与对话，落盘等于把隐私写进运维目录。
- **`max_tokens` 有下限（2048）+ 空正文自动加额**：上游把思考 token 算进输出额度。
  真号实测 `sn-sensenova-6-8-flash`：`max_tokens=16/512` 时思考吃满、正文空且
  `finish_reason=length`；768 起通常能出正文，但**同一句话的思考量能到 4000+ 字**，
  1024 仍会偶尔被吃光。所以两层防护：
  1. 出站下限抬到 2048（额度是上限不是花费，抬了不多扣）；
  2. 非流式检测到「正文空 + finish=length + 有思考」时，**自动把额度加到 8192 重试一次**；
     流式没法重试（思考帧已经发出去），在收尾补一帧
     `{"error":{"code":"reasoning_exhausted"}}`，让客户端看到原因而不是一条空消息。
- **`title` 只走 Header，不进 body**：真号实测 `raccoon-8c4485` 对未声明字段严格校验，
  body 里多一个 `title` 直接 HTTP 500 `Unexpected keyword argument 'title'`。
  网关把它摘出来、转成 `X-RACCOON-Title` 发出去——这一条 Header 会被上游写进
  积分流水的 `event_name`，于是每笔扣费都能反查到是哪次请求。
- **`temperature` 一律不外发**：上游对思考型模型（实测 `sn-kimi-k3`）只接受
  `temperature=1`，客户端带 0.7 这类默认值会被直接 HTTP 400
  `invalid temperature: only 1 is allowed for this model`（2026-11-02 线上，这一条
  占了当天失败的 57%）。温度对思考型模型本来不生效，网关统一摘掉字段、让上游用
  自己的默认值；代价是 `raccoon-*` 这类支持温度的模型也退回默认温度。
- **`max_tokens` 非法值两端都修**：客户端常拿 `-1` 表示「不限」，上游直接 400
  （文案有英文 `Range of max_tokens should be [1, 131072]`、中文
  `max_tokens参数非法：限制数值范围[1,131072]`、以及不带区间的
  `The request parameter max_tokens is invalid or missing` 三种）。所以负数/0 在
  出站前就抬到下限（2048）；超出报文区间时，按报文里的 `[lo, hi]` 收一次、**同号
  重试**，不换号也不罚号。

---

## 开发

```bash
go build ./...
go test ./...            # 用 httptest 假上游覆盖三协议 × 流/非流 × 工具调用
go test -race ./...
gofmt -l .               # CI 会拦未格式化文件
```

测试重点（`internal/server/e2e_test.go`）：换号、首帧错误、空流、401 续期、
模型别名、Anthropic/Responses 出站形状、工具调用 id 前缀、鉴权。

## 发版

push 到 `main` 自动 patch 自增 → 多平台二进制 → tag → Release。
bump 提交带 `[skip release]` 防循环。镜像由 `docker-ghcr.yml` 推送到 GHCR。

## License

MIT，见 [LICENSE](LICENSE)。

## 致谢

结构参考了同族项目 [workbuddy2api](https://github.com/rsfree/workbuddy2api) 的
「单二进制 + 内嵌面板 + 账号池」形态；小浣熊协议事实来自公开实现与抓包印证：

- [haoxinyu1/xiaohuanxiong2api](https://github.com/haoxinyu1/xiaohuanxiong2api)
- [misakano7545/xiaohuanxiong2api-panel](https://github.com/misakano7545/xiaohuanxiong2api-panel)
