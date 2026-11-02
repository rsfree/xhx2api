# 维护约定

## 发版

push 到 `main` 即自动发版（`.github/workflows/release.yml`）：

1. 读最近一个 `v*` tag，patch +1；
2. 跑测试 → 交叉编译 6 个平台；
3. 打 tag、建 Release（附件是 `xhx2api_<version>_<os>_<arch>.tar.gz/.zip`）；
4. 把新版本写回 `internal/version/version.go` 并提交，提交信息带 `[skip release]`
   防止「bump → 新 push → 再发版」无限循环。

镜像由 `.github/workflows/docker-ghcr.yml` 推 `ghcr.io/<owner>/xhx2api`，
tag 与 `latest`（默认分支）。

补发某个版本：`workflow_dispatch` 时填 `tag`（须已存在）。

## 代码约定

- 注释写「为什么」，不写「做了什么」——后者代码自己会说；
- 任何判错逻辑改动都要同时补 `internal/upstream` 的分类测试：误伤账号的代价
  远高于漏判（见 `TestClassifyDoesNotBlameAccountForParams`）；
- 错误分类只在 `upstream.Classify` 与 `classifyErrorText` 两处，别在 handler 里
  另写一份文案匹配；
- 配置/凭证/池状态的落盘统一是「写 `.tmp` → rename」：**配置只能目录挂载**
  （`./conf:/app/conf` + `-config /app/conf/config.json`）。挂成单文件
  `./config.json:/app/config.json` 时，`rename` 的目标是 bind mount 的 mountpoint，
  内核直接 `EBUSY`，面板「配置」页永远存不进去（2026-10-29 ai-prod 实测）。改挂载
  或 `-config` 路径时三处 compose 一起改：根、`deploy/`、`deploy/ai-prod/`；
- 面板改动后跑一次 `node --check internal/panel/app.js`，并至少手工验证
  「账号 / 模型 / 请求 / 配置」四页能渲染（无头 Chrome 即可）。
- 配置保存有**两种语义**，别混：`SaveConfig` 走合并（表单只提交它管的键，
  没提交的键保持磁盘旧值），`ReplaceConfig` 以提交为准（JSON 编辑器提交整份，
  没提到的已知键就是删除）。合并语义天然「只能加不能删」，所以集合型配置段
  （`setSectionKeys`，目前是 `aliases`）必须整段替换——否则面板里删掉一行、
  保存后又冒出来。`?mode=replace` 由前端 JSON 视图带上；replace 模式会拒空对象、
  且漏掉 `api_key` 时沿用旧值（否则会重新随机生成，全客户端 401）。

## 上游实测记录（改代码前先读）

2026-10-26 用真实账号（uid 7772869）跑 `cmd/livecheck` 得出的结论，
与公开参考实现有出入的地方都已修进代码：

| 现象 | 结论 | 落点 |
|---|---|---|
| `max_tokens:16/512` 正文回空、`finish_reason=length` | 思考 token 算在输出额度里；768 起稳定，1024 留余量 | `server/bodyfix.go` `liftMaxTokens` |
| body 里带 `title` → HTTP 500 `Unexpected keyword argument 'title'` | `raccoon-*` 对未知字段严格校验（`sn-*` 会忽略）；title 只能走 Header | `server/bodyfix.go` `stripTurnTitle` |
| 参数类错误以 5xx 返回 | 必须先判文案再判状态码，否则一次参数错误就熔断好号 | `upstream.Classify` 的 `paramErrorSignal` |
| 等响应头超时曾被当成「连不上」 | 一律记 server → handler 换号重试 3 次，一次请求白耗 360s，还给 3 个无辜账号各记一次失败。现按 `isTimeoutErr` 判成 gateway：504、不换号、不罚号 | `client.go` 的 `isTimeoutErr` / `timeoutMessage` |
| 流内空闲没有上限 | `upstream.idle_timeout_seconds` 一直是死配置（`IdleTimeout()` 无人调用），上游卡住不吐字节时请求能挂 877s，全程占着连接和在途名额 | `client.go` 的 `idleBody` 看门狗 |
| 单账号 + 首次 5xx 后返回「没有可用账号」 | 换号无候选时不要用 no_account 盖掉真实错误 | `handler.go` 的 `lastErr` 保留 |
| 桌面端 `agent/config/auth.json` 只有 `access_token`（IAM 票，30 天，无 refresh） | 「客户端目录」导入走这条路；覆盖导入必须继承旧 `refresh_token` + 余额/签到快照，否则账号会降级（`panel.mergeExisting`，测试 `TestMergeExistingKeepsSnapshot`） | `internal/clientdir`、`panel.mergeExisting` |
| 手机号登录三件套 `send_sms` / `login_with_sms` / `user_info` | phone/password 必须 AES-128-CFB 加密（`base64(iv‖ct)`，密钥 `senseraccoon2023`）；明文 → `params_encryted_error` | `upstream.EncryptCredential`、`authapi.go` |
| `send_sms` 的 `captcha_param` | 阿里云验证码（`scene=1pkmy0x3`/`prefix=hk1r5l`/`region=cn`），假票 → `100006 captcha_verify_error`，**绕不开**；面板 CSP 必须放行 `*.alicdn.com`，否则控件 `onError`（实测 o/g/x 三个 CDN 域名都要） | `panel.setSecurityHeaders`、`index.html#mode-sms` |
| 两条登录通道验证码不通用 | IAM（`/api/iam/v1/sms/send` + `/auth/login/sms`）与桌面端（`/api/web/auth/v1/send_sms` + `login_with_sms`）各发各的码；**发码与验码必须同通道**，混用报 `sms_code_verify_error`（实测：用户填对了也失败） | `upstream.SendSmsCode` 的 `preferLegacy`、`panel.smsPending` |
| 手机号登录拿到的 IAM 票是**未确认态** | 登录本身成功（有 access_token），但之后 `user_info` / `chat` 一律 HTTP 400 `{"code":200901,"message":"iam_confirm_required"}`。网页端在每个受保护请求前先 `POST /api/web/auth/v1/iam/confirm`（body `{}`，Bearer 票），拿到 `{"code":0}` 才继续；幂等，重复调用仍 code 0。不补这一步：`GetUserInfo` 会静默回落到 JWT 里的 `pu-…` 主体，账号看着导入成功、uid 是 `pu-…`、每个请求都 400（2026-10-29 真实号踩到） | `upstream.ConfirmIAM`、`LoginWithSms` 的 IAM 分支、`GetUserInfo` 的补确认重试 |
| uid 还是 `pu-…` = 资料没取到 | office 域 uid 是纯数字；`pu-` 前缀是 IAM 主体，说明 `user_info` 没通（未确认票/票过期），别当正常账号用 | 同上 |
| 「无续期令牌」不是坏号 | 只是没有 `refresh_token`（客户端目录 / 手机号导入的 IAM 票都只有 access_token，约 30 天）。号能正常用，到期前必须重新登录/导入 | `panel/app.js` 的 `has_refresh` 标签、`auth.Auth.RefreshToken` |
| 邀请归因字段 `inviter_user_id` | 只有桌面端通道认；IAM body 只有四个字段，多塞会被严格校验拒 | `upstream.LoginWithSms` |
| 邀请奖励 | **不存在**。老号流水零变化；新号只有 `新人注册礼包 3000` + `桌面端登录奖励 3000`；`/invite` 页面是组织邀请（拉人进团队），与积分无关 | — |
| 积分来源 | 发放制，无任务、无邀请返利：新人礼 3000 + 桌面端登录 3000 + **移动端登录 3000**（都可领）+ 每日 300 | `upstream.PointAudit` |
| 新号起始额度 | **9000 分**（注册礼 3000 自动 + 桌面端 3000 + 移动端 3000，后两笔入池即领） | `panel.claimWelcome` |
| 移动端奖励别等签到 | 实测注册当天就能领（6000→9000）；以前只挂在签到流程里，新号要等下一个时段 | `panel.claimWelcome` |
| 签到 `ineligible` 只能给「注册已过一天且流水从无 `daily_grant`」的号 | 新号当天 bills 里必然一条 `daily_grant` 都没有，先判「从来没有」就会把刚注册几小时的号报成 ineligible（2026-10-29 实测：手机号新号 17:07 注册、17:15 报「历史上从未获得每日积分」）。凭证里通常不带 `register_at`（裸 token / 手机号导入都没有），得用 `upstream.UserCreatedAt` 补一次并落盘；顺序必须是 今天注册→pending 先于 从无→ineligible | `scheduler.registerTime`、`scheduler.CheckinAccount`，测试 `TestCheckinTodayRegisteredIsPending` |
| 登录类错误以 HTTP 400 + 信封 code 返回 | 不能先看状态码，否则业务码被吞，面板只剩「http 400」看不懂 | `upstream.smsCall` / `smsBusinessError` |
| 上游 `visible:false` | 3 个 `raccoon-*` 全标 false，但可调用（默认模型 `raccoon-8c4485` 就是其中之一）。**不能按它过滤 /v1/models**，否则客户端看不到默认模型 | `server.ExcludeUpstreamHidden`（零值=全列）、`models.include_upstream_hidden` |
| `raccoon-chat-ml-5-5` | 不在目录里，但实测 HTTP 200；默认别名 `gpt-4o`/`claude-*` 指向它。模型页把它列为「目录外」 | `panel.models` 的 `orphans` |
| 前端 `$()` 是 querySelector | 必须写 `#id`；写裸 id 会被当标签名、静默返回 null（踩过：模型页整页空白）。`$` 已加「同 id 存在就告警」 | `panel/app.js` |
| 扣费口径 | **每请求最低 2 分**（16 token 也扣 2）；x1 约 **0.5 分/千 token**，按倍率线性缩放。实测 1,982 token(x1)=3 分、1,821 token(x0.2)=2 分 | `panel/app.js` 的 `RATE_X1_PER_KTOKEN`；改这个常数要同步 README |
| `raccoon-chat-ml-5-5` 计费 | 目录未公布倍率；1,990 token 扣 3 分，与 x1 的 `raccoon-8c4485` 一致 → 按 x1 估 | 模型页「目录外」卡片文案 |
| 思考吃光额度 | 同一 prompt 的思考量波动 90-4000+ token，`max_tokens=1024` 会偶发「空正文 + finish=length」。默认下限 2048；非流式自动加额到 8192 重试一次；流式补 `reasoning_exhausted` 错误帧 | `bodyfix.go`、`sse.go#finishStream`、`handler.go` 的 escalated |
| 模型特性 | `sn-*` 全为思考型（一句「收到」烧 21-224 思考 token，首字 5.6-17.4s）；`raccoon-*` 不思考、16 token 结束、首字 14-27s。首字最快 `sn-glm-5-3` | `livecheck -all-models` |
| `temperature` 只认 1 | `sn-kimi-k3` 带 0.7 直接 HTTP 400 `invalid temperature: only 1 is allowed for this model`（2026-11-02 线上：16 条＝当天 57% 的失败，下游还把 400 当可重试，3 分钟连打 16 次）。温度对思考型模型不生效，出站**一律摘掉**，不做「哪些模型只认 1」的白名单 | `bodyfix.go` 的 `stripTemperature`，测试 `TestStripTemperature` |
| `max_tokens` 超范围 | 400 `Range of max_tokens should be [1, 131072]`。上限随模型变（目录 `max_output_tokens=100000` 与校验上限并不一致），所以按**报错原文**取上界收一次、同号重试，不换号不罚号 | `handler.go` 的 `clamped`、`bodyfix.go` 的 `clampMaxTokens` |
| 账号失败率口径 | client/none 只记 `last_error`，不进 Total/Fail/FailCount、不参与平均延迟、不降权。否则下游传错参数就把好号喂到 `DegradeThresh`，账号页成功率也失去意义（历史累计的 429/503 仍会留在数字里） | `pool.release`，测试 `TestClientErrorNotCountedAgainstAccount` |
| 生图端点 | `POST {llm}/images/gen`，OpenAI Images 形状；**只有 `prompt` 必填**（空值 400 `RelayImageForm.Prompt required`）；`size`/`quality` 生效；上游只回 `data[0].b64_json`，**无 url** | `upstream.GenerateImage`、`server.imageGenerations` |
| image 字段形态 | 三种都收：单张字符串 / 字符串数组 / `null`（= 无图）；元素可为 data URI 或**裸 base64**（含 URL-safe、缺 padding）。`/v1/images/edits` 收 multipart 与 JSON；multipart 下 `image`/`image[]` 可为**文件**或**base64 文本**（同名重复即数组），`mask` 同理。数组里有非字符串 → 400（不静默丢图） | `extractImageRefs`、`decodeImageRef`、`imageEdits` |
| 生图响应形态 | **默认回 url**（不传 `response_format` / 传 `null`/`None` → `data[0].url`，不附 b64）；`response_format=b64_json` → 只回 b64 且**不落盘**；配置 `images.response_format=both` → 两者都给。落盘失败或无存储 → 回落 b64 + 头 `X-XHX-Image-URL: unavailable`，绝不回 `url:null` | `server.resolveResponseFormat`、`attachImageURLs` |
| 图片落盘与回收 | 内容寻址 `<sha256前16字节>.<ext>`，`GET /v1/images/<name>` 免鉴权、`immutable` 缓存；超过 `images.max_total_mb`（默认 2048，0=不限）按 mtime 删最旧到 90%，**只删 `imageNameOK` 认得的文件**，节流 1 分钟 | `sweepImageDir` |
| 生图 `model` 字段 | **完全被忽略**（2026-10-26 实测）：`gpt-image-2`、`gpt-image-2.5`、捏造名全 200，1024x1024 的 `image_tokens` 都是 196；响应体不回 `model`。别指望它切模型 | `upstream.ImageAliases` 注释 |
| 生图 size | **两条约束**：①最长边 ≤ 3840（超了先跑 106s 再 400）；②总像素 ≤ 8,294,400 = 3840×2160（超了**不报错**，静默降级：3840x3840 → 2880x2880）。实测 `1024x1024`→196 tokens、`1536x1024`→158、`1792x1024`→139、`3840x2160`(4K)→371 | `upstream.ClampImageSize` 提前按两条约束等比压，响应头 `X-XHX-Image-Size` |
| 4K + quality | `3840x2160&quality=high` → 13342 image_tokens（默认档 371，36 倍）、136s、1806KB；quality 对耗时几乎无影响 | 同上 |
| 生图默认值 | `size` 省略 → 上游给 **1254x1254**（229 tokens）；`size=auto` → **1024x1024**（196）——两者不一样。`quality` 省略恒为 `low` | `probe_4k.py ""` / `size=auto` |
| 生图并发 | 单账号 **10 上限**（与对话共用配额；⚠️ 2026-11-01 对话侧复测已降到 **5**，生图是否同步收紧未复测，按 5 规划更安全）：4 并发 4/4（167s）、8 并发 8/8（100s）、12 并发 10/12（2 个 429）。429 正文 `code 200103 limiter_request_error`、`retry after:0s`、0.1s 立即返回；**等 1s 重试 12/12 全成功** → `serveImage` 原地等 2s 重试，不换号不罚号 | `imageRateRetryDelay`、`pool.max_in_flight` |
| 生图混跑 | 8 生图 + 4 对话（同账号、对话延后 1s）12 个全过 → 429 看瞬时峰值，不是严格在途计数 | `img_mixed.py` |
| 生图全形态（线上） | 9 形态全过：文生图 1024/别名/`quality=high`/4K/超限压缩、图生图 multipart/mask/JSON 内联/多图。耗时 **49-267s**（267s 是 4096x4096 被压到 2880 后），故 `images.timeout_seconds` 默认提到 **600**。图生图确认真用输入图（蓝圆保留、方块按 prompt 由红改金）。tokens：文生图 220 / 图生图 1232 / 双图 2258 / high 7036 | `deploy/ai-prod`、`image_forms_e2e.py` |
| 生图计费（2026-10-27 受控复测） | 直连上游 66.3s/196 image_tokens、经网关 49.7s/213 tok，两个号余额**与 bill_id 集合均无变化** ⇒ **生图通道本身不扣分**。但**客户端 agent 画图会扣**（`任务消耗 · <会话标题>`，实测 -2/-4/-9）：那是 agent 的 LLM 轮次在花钱，不是生图。查扣费必须按 bill_id 求差集，只看余额会把别的客户端算到自己头上 | `/tmp/billprobe.py`、`/tmp/billdirect.py` |
| 图片模型组 | LiteLLM 侧是 `raccoon-image-gen`，fallback 为 `raccoon-4eb26a`/`raccoon-0c119c`/`raccoon-ecc5fd`；**`model_catalog` 不列图片模型**，所以网关合成 `raccoon-image-gen` 暴露给客户端 | `upstream.ImageModelID`、`/v1/models` |
| 生图别名 | `gpt-image-1/1-mini/2/2.5`、`dall-e-2/3` 默认全接受并出现在 `/v1/models`（`alias_of` 指回合成 id）；只为让硬编码这些名字的客户端能选中，不改实际模型。`images.aliases: []` 关掉（热改） | `upstream.ImageModelIDs`、`livecfg.Snapshot.ImageAliases` |
| **生图模型身份（C2PA 铁证）** | 上游是 **gpt-image-2**，**没有 Seedream、也没有商汤自家 U1 Pro**。19 张产物里 11 张盖章 `Azure OpenAI ImageGen`（Microsoft invismark + Microsoft SCD 证书链），5 张盖 `API` / 版本 `gpt-image-2` / `OpenAI Media Service API` / 签名主体 `OpenAI OpCo, LLC`（SSL.com C2PA ICA + OpenAI TSA 时间戳）。签名证书是厂商私钥，第三方模型伪造不了 | `data/images/*.png` 的 `caBX` 清单 |
| 两条渠道可辨形 | `Azure OpenAI` 恒为 `caBX` + **65536 字节分块 IDAT**；`OpenAI 直连` 恒为 `caBX` + **单 IDAT**（整块 1-2MB）。**无 C2PA 的 3 张全是单 IDAT** ⇒ 同源剥了签名块、图像未转码（JPEG 8x8 网格痕迹 0.93-1.14 ≈ 无），不是第三家模型 | 同上 |
| 生图尺寸是 OpenAI 规格 | `1536x1024`/`1024x1536` 原样返回（158 image_tokens）、`1024x1024` 196、不传 size → **1402x1122**（不再固定 1254x1254，上游自选比例）；`2048x2048` 直接 500。Seedream 的 2K/4K 固定档在这里不存在（4096² 会被压到 2880² 才是 gpt-image 的行为） | 尺寸矩阵实测 2026-10-27 |
| C2PA 取证位置 | C2PA 在 `caBX` 块，**位置在 IDAT 之前**；只扫到 IDAT 就收工会误判成「无元数据」（踩过：把 6 张带签的图判成无签名） | 扫块要走到 `IEND` |
| 生图耗时与容量 | 单张 20-200s；**与对话并发会抢账号容量**（实测生图 + raccoon-8c4485 并发 → 对话 180s 超时） | `images.timeout_seconds`（默认 300）、超时不换号 |
| 客户端生图是「可配置 OpenAI 端点」 | 桌面端内置预设指向 `${origin}/api/web/llm/v2/images/gen`，配置项 `{type:remote, endpoint, apiKey, model, timeout}`；说明**客户端把生图当 OpenAI 兼容接口用** | `build/electron/main/client/settings/builtinServiceSettings.js` |
| 上游并发限制 | **单账号 5 个在途**（2026-11-01 三账号两模型复测：c=6 → 5/6、c=12 → 5/12，第 6 个起 0.1s 内 429、不排队；按账号算、跨模型共享）。另有第二条闸：累计请求频率打满后**全模型** 429，≥30 分钟才解 | `pool.max_in_flight` 默认 5、`pool.queue_wait` 2s、`Pool.PickWait` |
| 扫码落地页 `/login/mp` 404 | 轮询端点还在（任意 code 回 pending），二维码改由上游生成；当前实现不可用 | 面板有提示；等逆向出新路径再修 |
| `X-RACCOON-Title` | 会写进积分流水 `event_name`（「任务消耗 · <title>」），可用来逐笔对账 | `upstream.ChatStream` |
| 一次 16 token 对话扣 2 分 | 倍率按模型（raccoon x1 / glm-5-3 x0.75 / glm-5-3-flash x0.2） | 面板模型页展示 |
| 限免模型 x0 | `sn-sensenova-6-8-flash` 等实测不扣积分（限免至 10-31） | 模型页标「免费」 |
| **429 不是 IP 维度** | 同一账号从两个出口 IP（`210.121.44.245` / `117.88.13.53`）各发 6 个请求，**全部 200**；51 并发集中打时 429 率反而只有 0.6%（31–38 在途 0.6%、39–46 在途 6.3%）。限流绑**账号**，不绑 IP——别用换代理/换机解决 | 2026-10-30 实测，`/tmp/xhxprobe.py` |
| **429 是秒级恢复的瞬时闸** | 被判「第 7 级退避、冷却 1.1h」的账号**直接打上游仍然 200**。原实现「429 → 账号级指数退避 60s→2h」把一次抖动放大成整池锁死：831 次 429 全是换号重试（attempts≥2），每次罚 2–3 个号，最终 7812 条 503（75%）。现改为**首次 429 原地等 2s 重试、不罚号不换号**，第 2 次才短冷却；基数 15s、封顶 5min | `server/handler.go` 的 `chatRateRetryDelay`、`pool.normalizeConfig`、`TestRotateOn429` / `TestRepeated429CoolsDown` / `TestSoftRateDefaultsAreMinutes` |

回归测试：`internal/upstream/upstream_test.go` 的 `TestClassify`（含 500+参数错误用例）、
`internal/server/bodyfix_test.go`（含 512/768 边界）。

## 数据与隐私

- `auths/`、`data/`、`config.json` 永不入库（`.gitignore` + `.dockerignore`）；
- 请求归档不含正文；
- 日志里不打印 token（用 `auth.MaskToken`）。
