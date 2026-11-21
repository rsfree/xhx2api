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
- **删配置项不能只删结构体字段**：`ParseConfig` 是 `DisallowUnknownFields`，老配置
  带着已删的键会在启动时直接报「配置项错误」——升级即停机。删字段的同一次改动里
  还要：登进 `cmd/server/config.go` 的 `deprecatedConfigKeys`（严格解析前摘掉、
  保存时从磁盘抹掉）、补一条「老配置能解析 + 未知键仍报错」的测试
  （见 `TestParseConfigAcceptsDeprecatedImagesModel`）；
- 配置/凭证/池状态的落盘统一是「写 `.tmp` → rename」：**配置只能目录挂载**
  （`./conf:/app/conf` + `-config /app/conf/config.json`）。挂成单文件
  `./config.json:/app/config.json` 时，`rename` 的目标是 bind mount 的 mountpoint，
  内核直接 `EBUSY`，面板「配置」页永远存不进去（2026-10-29 ai-prod 实测）。改挂载
  或 `-config` 路径时三处 compose 一起改：根、`deploy/`、`deploy/ai-prod/`；
- 面板改动后跑一次 `node --check internal/panel/app.js`，并至少手工验证
  「账号 / 模型 / 请求 / 配置」四页能渲染（无头 Chrome 即可）。
  **四页「结构在」不等于样式生效**：CSS 解析错误会让整块规则被静默丢弃。
  2026-11-02 实测踩过——文件头注释里写了 `.mc-*/.cfg-*`，其中的 `*/` 提前闭合了
  注释，`:root { … }` 整块令牌被浏览器丢掉；表现是浅色下所有卡片背景透明，
  而深色一切正常（`[data-theme="dark"]` 排在后面侥幸活下来），极易误判成
  「浅色主题写错了」。验证必须落到计算样式：
  `getComputedStyle(document.documentElement).getPropertyValue('--surface')` 非空、
  `.card` 的 `backgroundColor` 不是 `rgba(0, 0, 0, 0)`，明暗各看一遍。
  同理，注释里不要写 `*/` 相邻的字符组合。
- 账号表走「行签名 + 增量更新」（`accRowSig` / `renderAccounts`），不要退回
  `innerHTML = list.map(...)`：10 秒轮询整表重建会丢滚动位置、hover tooltip 和
  正在选中的文本。**新增会显示在行里的字段必须同时进签名**，否则会出现
  「数据变了但那一行没重绘」。行的顺序变化用 `insertBefore` 移动节点，保留节点身份。
- 账号表的筛选/排序/分页全在内存里做（数据源是 overview 快照），顺序固定为
  「过滤 → 排序 → 分页」；先分页会让「第 2 页」的含义随排序漂移。状态口径只有
  `accStatus()` 一处，筛选、异常汇总条、排序分组共用它——各写一份必然漂移成
  「条上数了 3 个冷却，点进去只有 2 行」。
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
| 上游 `paging.offset` 是死参数 | `offset>0` 恒返回空数组（offset=1/50/100 全 0 条），`page`/`page_size` 被完全忽略 ⇒ `type=all` **永远只有最近 100 条**。重度账号一天几十上百条消费，100 条只覆盖 1~1.5 天 | `client.BillsQuery` 注释；判签到改用 `BillsIncomeQuery` |
| 判签到必须用 `type=income` | 只返回发放行，不受 100 条窗口限制（同一号：income 覆盖 10-26→11-02，all 只覆盖 11-01→11-02）。用 all 判会让「10-29/30/31 连拿三天 300 分」的号被判「历史上从未获得」→ 按无资格跳过、永远不再签（2026-11-02 线上实测 7812474） | `upstream.DailyGrant`、`dailyBatchExpiry`、`earliestBatchExpiry`；测试 `TestDailyGrantUsesIncomeQuery` |
| `POST /login/points/grant` 是纯空操作 | 11 个号 × 5 种身份头（desktop-win/mac/web/app-android/无版本头）全回 `{"granted":false}`，调完 bills 里 `daily_grant` 一条不涨。**它不是发分入口**，别指望「调一下就有分」 | `upstream.CheckinClaim`（只当观测用） |
| 每日 +300 的发放时点不固定 | 实测出现 00:00 / 00:21 / 00:55 / 09:25 / 13:30 / 14:36，不是「服务端 00:00 定时任务」（旧注释写错了）。所以只能在事后按 bills 观测，不能定时「触发」 | `scheduler` 包注释 |
| **有没有每日积分由「可信设备绑定」决定** | `user_info.trusted_device.bound`：2026-11-02 实测 11 个号里 9 个 `bound=false`（`reason=session_without_bound_device`）**从来没拿到过一次** `daily_grant`，2 个 `bound=true` 的天天有 300。裸 token / 手机号导入的号都落在 unbound ⇒ 这批号**永远**不会有每日积分，`claim` 再多轮也没用。绑定端点全 404（`auth/v1/device*`、`desktop/v1/device*`、各种 bind/register 路径都试过；该服务对 method 不匹配也回 404 而非 405，所以 DELETE 探测法同样失效）。**device_id 是服务端生成的**：自造 `x-device-id` / `X-Device-Id` / `x-raccoon-device-id` 三种头实测全部被忽略，`device_id` 仍为空。当前可用的路只有「让登录请求走桌面端通道」（见下一行） | `upstream.UserProfile`、`scheduler.profile`、`auth.Data.DeviceBound`；测试 `TestCheckinUnboundDeviceReportsUnbound` |
| 未绑设备不能记「失败」也不能记「上游未开通」 | 报 `unbound` + 面板「未绑设备」标签，批量签到归 **Skipped**（归 Failed 会让面板每天一条红字，把真正的网络/凭证错误淹掉）。文案要说清「去官方客户端登录一次完成绑定」 | `scheduler.CheckinAccount`/`checkinAll`、`panel/app.js`；测试 `TestCheckinUnboundCountedAsSkipped` |
| **补绑设备有独立接口，不必重新登录** | `POST /api/web/auth/v1/devices_current_bind`。2026-11-03 实测：对 7822671 调用后立刻返回 `device_id=dev65f4f7…` + `trusted_device:true`，再查 `user_info` 当场变 `bound=true` / `needs_bind=false` / `reason` 空。**路径靠猜是猜不到的**：真实写法是 `devices_current_bind`（复数+下划线），所有 `device/bind`、`device_bind`、`desktop/v1/device*` 全是 404。来源 = 官方桌面客户端 `app.asar` 里的 `DesktopTrustedDeviceApi.bindCurrentDevice()`（同模块还有 `/devices_current_heartbeat`） | `upstream.BindDevice`（`internal/upstream/device.go`）、面板「补绑设备」；测试 `TestBindDeviceSendsOfficialShape` |
| 绑定请求的固定口径（照抄官方客户端） | body 九个字段：`client_device_id_source=unknown`、`client_device_id`(UUIDv4)、`client_platform=desktop-windows`、`client_version`、`device_name`、`os=windows`、`os_version`、`application=desktop`、`push_permission=unknown`；头必须带 `X-Client-Platform`(无 -x64 后缀) / `X-Client-Device-ID` / `X-Client-Version` / `X-Request-Id` | 同上 |
| `client_device_id` 必须持久化复用 | 它等价于「一台机器」。每次绑都换 UUID = 同一账号不断声明新设备，正是风控最敏感的模式。落盘在 `auth.Data.ClientDeviceID`，重启也要还原（`auth.Merge` 里少还原一处就等于换机器）。用 `crypto/rand` 生成，**不要从 uid 派生**——那等于把「同一批号」写进标识里 | `auth.Data.ClientDeviceID`、`newClientDeviceID`；测试 `TestBindDeviceReusesClientDeviceID` |
| 登录通道与能不能绑设备**无关** | 早先推断「桌面端通道才建设备、IAM 不建」——现在看这个推断不成立：绑定是独立接口，跟走哪条通道没关系。7812474 走 IAM 却 bound=true，正是因为它可能在客户端里绑过。所以面板登录通道默认仍用已验证的 `iam`，绑定交给「补绑设备」按钮 | `panel` 的 `login_via` |
| 设备态查不到 ≠ 无资格 | `user_info` 打不通且无缓存时必须报 `pending`（待定），不能退回 `ineligible`——把网络抖动说成账号无资格是谎报 | `scheduler.profile` 的 `known` 返回值；测试 `TestCheckinDeviceUnknownStaysPending` |
| **出口 IP 是按「账号」被风控的，不是按凭证** | 2026-11-02 ai-prod 一次性跑 11 个号签到后，`/api/web/points/` 整个前缀回 403（空 body + `X-TT-System-Error: 3` + `Proxy-Status`），同 token 在本地/jump 均 200；**连不带 token 的请求也 403** ⇒ 拦截发生在认证之前，与凭证无关。`llm` / `auth` 域同时正常（聊天实测 200） | `upstream.ProxyConfig`、`client.clientFor` |
| 代理必须按账号固定出口 | 供应商按**用户名**做粘性：`socks5h://poolclient-{account}:pass@host:port` 里 `{account}` 换成 uid，同一 uid 恒定同一出口（实测 7772869 → 64.81.112.31，7774611 → 154.40.45.35，重复查不变）。`http.Transport.DialContext` 拿不到请求信息 ⇒ **必须按账号各建一条 transport**，不能共享 | `proxyManager.transportFor`；测试 `TestProxyRoutesByAccount` |
| 代理模板不能直接丢给 `net/url` | `url.Parse` 对 userinfo 里的 `{}` 报 `invalid userinfo`，而模板本身就带 `{account}` ⇒ 必须先把占位符换成纯字母再解析、解析后换回（2026-11-03 实测踩到，现象是「模板无效」但看不出是花括号的问题） | `parseProxyTemplate` |
| 代理连不通**不能**判成 `server` | `server` 会累加 `FailCount`，3 次触发熔断 ⇒ 一次代理抖动就能把好号全打进熔断，等于「链路故障 → 账号全灭」。代理拨号失败包成 `proxyDialError`，判 `ErrGateway`（Minor：换号不罚号） | `proxyDialError`、`client.do`/`ChatStream`；测试 `TestProxyDialFailureIsGatewayNotServer` |
| 代理出口会**临时**建不起来，不要一次取样就下结论 | 2026-11-03 00:33 实测 11 个用户名里只有 2 个能建隧道，其余回 SOCKS5 码 3，当时判定「供应商白名单没开通」——**错了**。00:58 重测，11 个全部拿到各自独立出口（9 个在 38.6.213.x，互不相同），同一用户名两次探测结果恒定（粘性成立）。真正的解释是代理侧**临时容量/并发限制**。判据：当时连 `1.1.1.1` 都不通，恢复后同一用户名一切正常 ⇒ 与用户名、DNS、目标站都无关 | `proxyManager`；下次遇到先等几分钟重测，别急着报「供应商没开通」 |
| 出口 IP 不是「海外就不可用」 | 能通的出口本身就是海外：64.81.112.31（东京）、154.40.45.35（香港）、38.6.213.x（西雅图），打国内站都正常。判断"是不是地理问题"要看**同一用户名对不同目标**的表现，而不是看 IP 归属 | — |
| 代理失败必须回落直连 + 按账号冷却 | 9 个号直连本来 200，被强制走代理后全部失败 = 自己制造的降级。`retryDirect` 当场用共享 client 重发（**必须用 GetBody 重建 body**，否则空 body 会被上游当参数错误）；`proxyFailCooldown=5min` 让后续请求直接走直连，不再撞建连超时，到期自动重试 | `client.retryDirect`、`proxyManager.inFailCooldown`/`usableFor`；测试 `TestProxyDialFailureFallsBackToDirect`、`TestProxyFailCooldownSkipsProxy` |
| 代理作用域默认只覆盖积分域 | 供应商往往只给部分用户名开通（实测 11 个号里只有 2 个能建隧道，其余 SOCKS5 码 3 = network unreachable）。全局启用会让另外 9 个号的**聊天**也断，而聊天直连本来是好的。`scope=points`（默认）只让 `/api/web/points/` 与 `/points/grant` 走代理 | `ProxyConfig.appliesTo`；测试 `TestProxyScopeDefaultsToPoints` |
| 日志要能直接对上「哪个出口出事」 | 启动时逐账号打印 `[proxy] 账号=X 来源=user:***@host:port 出口=IP`；失败时打一行 `[proxy] 账号=… 来源=… 出口=… METHOD /path -> HTTP 403`。密码一律脱敏；出口查不到显示 `?` 而不是编一个地址 | `LogProxyEgress`、`logProxiedFailure`、`ProxyRoute` |
| 登录类错误以 HTTP 400 + 信封 code 返回 | 不能先看状态码，否则业务码被吞，面板只剩「http 400」看不懂 | `upstream.smsCall` / `smsBusinessError` |
| 上游 `visible:false` | 3 个 `raccoon-*` 全标 false，但可调用（默认模型 `raccoon-8c4485` 就是其中之一）。**不能按它过滤 /v1/models**，否则客户端看不到默认模型 | `server.ExcludeUpstreamHidden`（零值=全列）、`models.include_upstream_hidden` |
| `raccoon-chat-ml-5-5` | 不在目录里，但实测 HTTP 200；默认别名 `gpt-4o`/`claude-*` 指向它。模型页把它列为「目录外」 | `panel.models` 的 `orphans` |
| 前端 `$()` 是 querySelector | 必须写 `#id`；写裸 id 会被当标签名、静默返回 null（踩过：模型页整页空白）。`$` 已加「同 id 存在就告警」 | `panel/app.js` |
| 扣费口径 | **每请求最低 2 分**（16 token 也扣 2）；x1 约 **0.5 分/千 token**，按倍率线性缩放。实测 1,982 token(x1)=3 分、1,821 token(x0.2)=2 分 | `panel/app.js` 的 `RATE_X1_PER_KTOKEN`；改这个常数要同步 README |
| `raccoon-chat-ml-5-5` 计费 | 目录未公布倍率；1,990 token 扣 3 分，与 x1 的 `raccoon-8c4485` 一致 → 按 x1 估 | 模型页「目录外」卡片文案 |
| 思考吃光额度 | 同一 prompt 的思考量波动 90-4000+ token，`max_tokens=1024` 会偶发「空正文 + finish=length」。默认下限 2048；非流式自动加额到 8192 重试一次；流式补 `reasoning_exhausted` 错误帧 | `bodyfix.go`、`sse.go#finishStream`、`handler.go` 的 escalated |
| 模型特性 | `sn-*` 全为思考型（一句「收到」烧 21-224 思考 token，首字 5.6-17.4s）；`raccoon-*` 不思考、16 token 结束、首字 14-27s。首字最快 `sn-glm-5-3` | `livecheck -all-models` |
| `temperature` 只认 1 | `sn-kimi-k3` 带 0.7 直接 HTTP 400 `invalid temperature: only 1 is allowed for this model`（2026-11-02 线上：16 条＝当天 57% 的失败，下游还把 400 当可重试，3 分钟连打 16 次）。温度对思考型模型不生效，出站**一律摘掉**，不做「哪些模型只认 1」的白名单 | `bodyfix.go` 的 `stripTemperature`，测试 `TestStripTemperature` |
| `max_tokens` 不合法 | 三种文案都见过（2026-11-02 线上）：`Range of max_tokens should be [1, 131072]`、`max_tokens参数非法：限制数值范围[1,131072]`、`The request parameter max_tokens is invalid or missing`。负数是客户端当「不限」传的，**必须在出站前抬到下限**（第三种文案根本没区间可解析）；超上限再按报文里的 `[lo, hi]` 收一次、同号重试，不换号不罚号。区间随模型变（目录 `max_output_tokens=100000` 与校验上限不一致），别硬编码 | `bodyfix.go` 的 `liftMaxTokens` / `clampMaxTokens`、`handler.go` 的 `clamped` |
| 账号失败率口径 | client/none 只记 `last_error`，不进 Total/Fail/FailCount、不参与平均延迟、不降权。否则下游传错参数就把好号喂到 `DegradeThresh`，账号页成功率也失去意义（历史累计的 429/503 仍会留在数字里） | `pool.release`，测试 `TestClientErrorNotCountedAgainstAccount` |
| 生图端点 | `POST {llm}/images/gen`，OpenAI Images 形状；**只有 `prompt` 必填**（空值 400 `RelayImageForm.Prompt required`）；`size`/`quality` 生效；上游只回 `data[0].b64_json`，**无 url** | `upstream.GenerateImage`、`server.imageGenerations` |
| image 字段形态 | 三种都收：单张字符串 / 字符串数组 / `null`（= 无图）；元素可为 data URI 或**裸 base64**（含 URL-safe、缺 padding）。`/v1/images/edits` 收 multipart 与 JSON；multipart 下 `image`/`image[]` 可为**文件**或**base64 文本**（同名重复即数组），`mask` 同理。数组里有非字符串 → 400（不静默丢图） | `extractImageRefs`、`decodeImageRef`、`imageEdits` |
| 生图响应形态 | **默认回 url**（不传 `response_format` / 传 `null`/`None` → `data[0].url`，不附 b64）；`response_format=b64_json` → 只回 b64 且**不落盘**；配置 `images.response_format=both` → 两者都给。落盘失败或无存储 → 回落 b64 + 头 `X-XHX-Image-URL: unavailable`，绝不回 `url:null` | `server.resolveResponseFormat`、`attachImageURLs` |
| 图片落盘与回收 | 内容寻址 `<sha256前16字节>.<ext>`，`GET /v1/images/<name>` 免鉴权、`immutable` 缓存；超过 `images.max_total_mb`（默认 2048，0=不限）按 mtime 删最旧到 90%，**只删 `imageNameOK` 认得的文件**，节流 1 分钟 | `sweepImageDir` |
| 生图 `model` 字段 | **完全被忽略**（2026-10-26 实测）：`gpt-image-2`、`gpt-image-2.5`、捏造名全 200，1024x1024 的 `image_tokens` 都是 196；响应体不回 `model`。别指望它切模型，也**别加模型名校验**：按端点路由，空串/别名/对话模型名/随手编的名字都出图（`TestImageEndpointIgnoresModelName`）。`images.model` 配置已删除——上游没有第二个模型可选；老配置里的键由 `deprecatedConfigKeys` 忽略并在保存时抹掉 | `upstream.ImageAliases` 注释、`cmd/server/config.go` |
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
