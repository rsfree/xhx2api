# xhx2api · ai-prod 运维说明

公网入口：**https://xhx2api.1task.cn** 　面板：https://xhx2api.1task.cn/panel/
本机回环：`127.0.0.1:39026` → 容器 `7867` 　源：github.com/rsfree/xhx2api

## 目录

```
/opt/xhx2api/
├── docker-compose.yml     # 镜像部署声明（端口只绑回环）
├── .env                   # XHX2API_IMAGE_TAG=sha-<提交>
├── conf/
│   └── config.json        # 生产配置（含 api_key，600，属主 10001）—— 目录挂载，不挂单文件
├── auths/                 # 账号凭证（JWT/refresh，700，属主 10001）
├── data/                  # 池状态 + 用量 + 请求日志 + 模型目录缓存
│   └── images/            # 生成的图（默认返回 url 后每张都落盘，600/属主 10001）
└── nginx/                 # 站点配置与签发脚本（真源在仓库 deploy/）
```

> 🔴 配置是 `./conf:/app/conf` **目录挂载** + `-config /app/conf/config.json`，
> 不是单文件挂载。原因：面板保存用「写 `config.json.tmp` → `rename` 覆盖」，
> `rename` 的目标若是 bind mount 的 mountpoint，内核返回 `EBUSY`，保存必失败。

## 日常操作

```bash
cd /opt/xhx2api

docker compose ps                    # 状态（应 Up (healthy)）
docker logs --tail 50 xhx2api        # 日志
curl -s http://127.0.0.1:39026/healthz   # 存活（恒 200）

docker compose restart xhx2api       # 重启（会先落盘池状态）
docker compose pull && docker compose up -d   # 拉新镜像并换
```

## 升级（CI 出镜像后）

镜像由仓库的 `docker` workflow 在 push main 时构建，tag 为
`main` / `latest` / `sha-<7位>`。**回滚靠 sha tag，不靠 latest**：

```bash
cd /opt/xhx2api
# 1) 看有哪些版本
T=$(curl -s "https://ghcr.io/token?scope=repository:rsfree/xhx2api:pull&service=ghcr.io" \
    | python3 -c "import sys,json;print(json.load(sys.stdin)['token'])")
curl -s -H "Authorization: Bearer $T" https://ghcr.io/v2/rsfree/xhx2api/tags/list

# 2) 钉住某个提交（把 .env 里那行改掉）
sed -i 's/^XHX2API_IMAGE_TAG=.*/XHX2API_IMAGE_TAG=sha-02d21d1/' .env
docker compose pull && docker compose up -d
```

版本号会体现在面板顶部（镜像构建时注入 `VERSION`，非 tag 构建为 `dev-<sha>`）。

## 证书

```bash
bash /opt/xhx2api/nginx/apply-https.sh    # 幂等：已有证书则只做安装/续期配置
```

acme.sh 自动续期，`reloadcmd` 已配。到期前 30 天会自动续，无需人工干预。

## 备份

`auths` 与 `data` 是**有状态**的，升级/迁移前先备份。注意 `data/images` 可能有几个 GB
（默认上限 2GB），只想留状态就别把图打包进去：

```bash
tar czf /root/xhx2api-backup-$(date +%F).tar.gz -C /opt/xhx2api auths conf \
  --exclude='data/images' data
```

## 配置挂载迁移（单文件 → 目录，2026-10-29）

老部署把配置单文件挂到 `/app/config.json`，导致面板「配置」页保存必然报
`rename /app/config.json.tmp /app/config.json: device or resource busy`（配置不变）。

按顺序执行，**先 copy 再起容器**——顺序错了会挂载空目录，程序自动生成一份
带**新随机 api_key** 的配置，所有客户端立刻鉴权失败：

```bash
cd /opt/xhx2api
# 0) 备份（含旧 config.json）
tar czf /root/xhx2api-preconf-$(date +%F-%H%M).tar.gz config.json auths data --exclude='data/images'

# 1) 建目录并复制配置，属主必须 10001
mkdir -p conf
cp -a config.json conf/config.json
chown -R 10001:10001 conf
chmod 600 conf/config.json

# 2) 换 compose（新文件含 entrypoint + ./conf:/app/conf）
#    确认里面只有 ./conf:/app/conf，没有 ./config.json:/app/config.json
grep -n 'conf\|config.json' docker-compose.yml

# 3) 重建容器
docker compose up -d
docker compose ps                 # 应 Up (healthy)
```

验收（三条都要过）：

```bash
# ① 挂载点变成目录
docker exec xhx2api sh -c 'cat /proc/mounts | grep /app/conf'
# ② rename 合法（这是原先 EBUSY 的操作）
docker exec xhx2api sh -c 'echo x > /app/conf/.rename-probe && mv /app/conf/.rename-probe /app/conf/.rename-ok && echo RENAME_OK; rm -f /app/conf/.rename-ok'
# ③ api_key 没变（对比备份里的旧值）
grep -o '"api_key": *"[^"]*"' conf/config.json
tar xzOf /root/xhx2api-preconf-*.tar.gz config.json | grep -o '"api_key": *"[^"]*"'
```

最后进面板「配置」页点一次保存，看到「已保存」即修复完成；旧的
`/opt/xhx2api/config.json` 已不再被挂载，留作备份即可，别再挂回去。

## 图片目录（默认开）

`images.return_url` 默认 **true**：每张生成的图都会落盘到 `data/images/`，并提供
`https://xhx2api.1task.cn/v1/images/<sha256前16位>.<ext>`。要点：

- 文件名是**内容哈希**，同一张图不会重复落盘；
- 容量上限 `images.max_total_mb`（默认 2048，`0` = 不限）：超了按 mtime 删最旧的到 90%，
  **旧链接会 404**，别当永久存储；
- 想彻底不落盘：`images.return_url=false` + `images.response_format=b64_json`
  （改了要重启，重启后用 `docker exec xhx2api sh -c 'touch /app/data/images/.w'` 验可写）；
- 目录属主必须 10001，否则容器写不进去 → 响应会回落成 base64（`X-XHX-Image-URL: unavailable`）：

```bash
chown -R 10001:10001 /opt/xhx2api/data && chmod 755 /opt/xhx2api/data/images
```

## 注意

- **同一账号不要同时在两处跑**：本机和线上若挂同一批凭证，refresh token 轮换会互相顶掉，
  表现为随机掉登录。迁移时先把旧实例停掉。
- `auths/*.json` 等同明文密码，不要进 git、不要贴聊天记录。
- nginx 的 `proxy_read_timeout 3600s` 是给生图和慢对话留的：上游实测同一模型
  12s→137s 波动，默认 60s 会掐断，表现为「网关卡死」。
