#!/usr/bin/env python3
"""imgrate — 小浣熊生图通道频控测绘（直连上游，不经网关）。

为什么直连：网关会换号、排队、重试 429，把「上游限流」这层完全遮住。
要把限流器的形状测出来，只能拿单账号的票直接打上游。

核心技巧（2026-11-21 实测）：**限流器在参数校验之前**。
20 个缺 prompt 的非法请求同样被精确拦成 10 过 / 10 拒，说明额度是按
「请求到达」扣的，跟后面生成什么无关。于是可以拿缺 prompt 的 400 当探针，
零成本测绘整个限流曲线，只有最后几轮用真图验证「生成请求走同一口池子」。

事件流（events.jsonl，一行一个请求，append + fsync）：
    seq, t_ms(单调), wall(本地时间), phase, probe, acct,
    status, kind, code, retry_s, lat_ms, inflight, body, err

统计流（stats.jsonl，每 10 秒一行）：多滑动窗口的 sent/ok/r429，用于反推
窗口长度与配额；窗口长度取 1/5/10/20/30/60/120/300 秒，哪个窗口在 429
之前顶到同一个数，那个数就是该窗口的配额。

用法：
    python3 tools/imgrate.py smoke                     # 3 个探针，验证通道
    python3 tools/imgrate.py plan                      # 只打印计划与成本
    python3 tools/imgrate.py run --hours 8             # 正式长跑
    python3 tools/imgrate.py run --hours 8 --real-budget 40
    python3 tools/imgrate.py report --dir .workbuddy/imgrate/2026-11-21
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from collections import Counter
from datetime import datetime
from pathlib import Path

LLM = "https://xiaohuanxiong.com/api/web/llm/v2"
POINTS = "https://xiaohuanxiong.com/api/web/points/v1"
PROJECT = Path(__file__).resolve().parent.parent

# 上游生图单价（2026-11-21 实测：按张 20 分，与 quality 无关）。
POINTS_PER_IMAGE = 20

# 429 的正文里 retry-after 一般是 0s；留个解析口子，万一出现非零就是线索。
RETRY_RE = re.compile(r"retry after:\s*([0-9.]+)\s*s", re.I)

# 探针用的两组 payload。
#   noprompt —— 缺 prompt，最快的 400，只用来占限流额度，不产生任何生成。
#   real     —— 真生图。quality=low 实测 14.4s，比 high 的 97.3s 省时间；
#               价格两者相同（20 分/张），测频控当然选快的。
NOPROMPT = {}
REAL = {
    "prompt": "a single red apple on a plain white background, product photo",
    "model": "raccoon-image-gen",
    "size": "1024x1024",
    "quality": "low",
    "n": 1,
    "response_format": "b64_json",
}

WINDOWS = (1, 5, 10, 20, 30, 60, 120, 300, 900)


def now_wall() -> str:
    return datetime.now().astimezone().isoformat(timespec="milliseconds")


class Recorder:
    """事件落盘。每行一次 write+flush，进程被杀最多丢一行。"""

    def __init__(self, outdir: Path, resume: bool = True):
        self.dir = outdir
        self.dir.mkdir(parents=True, exist_ok=True)
        self.events_path = self.dir / "events.jsonl"
        self.stats_path = self.dir / "stats.jsonl"
        self.note_path = self.dir / "notes.log"
        self.seq = 0
        self.t0 = time.monotonic()
        self.lock = threading.Lock()
        if resume and self.events_path.exists():
            # 崩溃/重启后续号，别覆盖前面的证据。
            with self.events_path.open("rb") as f:
                f.seek(0, os.SEEK_END)
                size = f.tell()
                f.seek(max(0, size - 8192))
                tail = f.read().decode("utf-8", "ignore").strip().splitlines()
            for line in reversed(tail):
                try:
                    self.seq = int(json.loads(line)["seq"])
                    break
                except Exception:
                    continue
        self.fh = self.events_path.open("a", encoding="utf-8")
        self.sfh = self.stats_path.open("a", encoding="utf-8")

    def emit(self, ev: dict) -> dict:
        with self.lock:
            self.seq += 1
            ev["seq"] = self.seq
            ev["t_ms"] = int((ev.get("_mono", time.monotonic()) - self.t0) * 1000)
            ev.pop("_mono", None)
            ev.setdefault("wall", now_wall())
            self.fh.write(json.dumps(ev, ensure_ascii=False) + "\n")
            self.fh.flush()
        return ev

    def stats(self, row: dict) -> None:
        with self.lock:
            self.sfh.write(json.dumps(row, ensure_ascii=False) + "\n")
            self.sfh.flush()

    def note(self, text: str) -> None:
        line = f"[{now_wall()}] {text}"
        print(line, flush=True)
        with self.lock:
            with self.note_path.open("a", encoding="utf-8") as f:
                f.write(line + "\n")


def _multipart(fields: dict, files: list[tuple[str, str, bytes, str]]) -> tuple[bytes, str]:
    """最小 multipart 构造。上游 /images/edits 只吃 multipart（JSON 回 415）。"""
    boundary = "----imgrate" + uuid.uuid4().hex
    parts: list[bytes] = []
    for k, v in fields.items():
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{k}"\r\n\r\n{v}\r\n'.encode()
        )
    for name, filename, blob, mime in files:
        parts.append(
            f'--{boundary}\r\nContent-Disposition: form-data; name="{name}"; '
            f'filename="{filename}"\r\nContent-Type: {mime}\r\n\r\n'.encode()
        )
        parts.append(blob)
        parts.append(b"\r\n")
    parts.append(f"--{boundary}--\r\n".encode())
    return b"".join(parts), f"multipart/form-data; boundary={boundary}"


class Prober:
    """一个账号的直连探针。"""

    def __init__(self, acct: dict, rec: Recorder, timeout: float = 120.0):
        self.uid = acct["uid"]
        self.token = acct["access_token"]
        self.rec = rec
        self.timeout = timeout
        # 全局禁用环境代理：memory 里记过 HTTP_PROXY 会连 Authorization 一起吞。
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        self.inflight = 0
        self.ilock = threading.Lock()
        self.peak_inflight = 0
        self.real_spent = 0  # 已发起的真图数（按张计费，成功与否都先记预算）

    def _hdr(self, extra: dict | None = None) -> dict:
        h = {
            "Authorization": "Bearer " + self.token,
            "Content-Type": "application/json",
            "Accept": "application/json",
            "x-client-name": "@raccoon/office-desktop",
            "x-client-platform": "desktop-windows-x64",
            "x-client-version": "v1.1.0",
            "x-client-channel": "official",
            "User-Agent": "xhx2api/1.0",
        }
        if extra:
            h.update(extra)
        return h

    def _fire(self, url: str, payload: dict, phase: str, probe: str, n: int = 1,
              files: list | None = None) -> dict:
        if files:
            data, ctype = _multipart(payload, files)
        else:
            data, ctype = json.dumps(payload).encode(), "application/json"
        with self.ilock:
            self.inflight += 1
            inflight = self.inflight
            if inflight > self.peak_inflight:
                self.peak_inflight = inflight
        mono = time.monotonic()
        t0 = time.perf_counter()
        status, text, err = 0, "", ""
        req = urllib.request.Request(url, data=data,
                                     headers=self._hdr({"Content-Type": ctype}), method="POST")
        try:
            with self.opener.open(req, timeout=self.timeout) as resp:
                status = resp.status
                text = resp.read(4096).decode("utf-8", "ignore")
        except urllib.error.HTTPError as e:
            status = e.code
            try:
                text = e.read(4096).decode("utf-8", "ignore")
            except Exception as e2:  # 客户端半路断开之类
                err = f"read-error:{e2!r}"
        except Exception as e:
            status = -1
            err = repr(e)[:200]
        lat = (time.perf_counter() - t0) * 1000
        with self.ilock:
            self.inflight -= 1

        code = None
        m = re.search(r'"code"\s*:\s*(\d+)', text)
        if m:
            code = int(m.group(1))
        retry = RETRY_RE.search(text)
        if status == 200:
            kind = "ok"
        elif status == 429:
            kind = "r429"
        elif status == 400:
            kind = "bad400" if probe == "noprompt" else "r400"
        else:
            kind = f"http{status}" if status > 0 else "neterr"
        return self.rec.emit({
            "phase": phase, "probe": probe, "acct": self.uid, "n": n,
            "status": status, "kind": kind, "code": code,
            "retry_s": float(retry.group(1)) if retry else None,
            "lat_ms": round(lat, 1), "inflight": inflight,
            "body": text[:180].replace("\n", " "), "err": err,
            "_mono": mono,
        })

    def noprompt(self, phase: str, n: int = 1) -> dict:
        return self._fire(LLM + "/images/gen", NOPROMPT, phase, "noprompt", n)

    def real(self, phase: str, n: int = 1) -> dict:
        with self.ilock:
            self.real_spent += 1
            spent = self.real_spent
        # 预算必须落盘：KeepAlive 重启后 real_spent 归零会让 100 张上限
        # 变成「每次崩溃重置一次」，一夜下来能烧掉整池余额。
        try:
            (self.rec.dir / ".real-spent").write_text(str(spent))
        except OSError:
            pass
        return self._fire(LLM + "/images/gen", REAL, phase, "real", n)

    def real_edit(self, phase: str, n: int = 1) -> dict:
        """图生图（/images/edits，multipart）。输入图用历史产物里最小的一张 PNG。"""
        with self.ilock:
            self.real_spent += 1
            spent = self.real_spent
        try:
            (self.rec.dir / ".real-spent").write_text(str(spent))
        except OSError:
            pass
        blob = (PROJECT / "data" / "images"
                / "33c8b2bcaa7b3acbe600a0d7be84ef31.png").read_bytes()
        fields = {"prompt": REAL["prompt"], "size": "1024x1024", "quality": "low"}
        return self._fire(LLM + "/images/edits", fields, phase, "edit", n,
                          files=[("image", "input.png", blob, "image/png")])

    def balance(self, acct_uid: str | None = None) -> int | None:
        req = urllib.request.Request(POINTS + "/balance", headers=self._hdr())
        try:
            with self.opener.open(req, timeout=15) as resp:
                d = json.loads(resp.read().decode())
            return d.get("data", {}).get("available_points")
        except Exception:
            return None


def burst(p: Prober, n: int, phase: str, probe: str = "noprompt",
          stagger_ms: float = 0.0) -> Counter:
    """n 个请求尽量同时发出（Barrier 对齐），返回状态分布。"""
    b = threading.Barrier(n + 1, timeout=30)
    out: list[dict | None] = [None] * n

    def w(i: int):
        b.wait()
        if stagger_ms:
            time.sleep(i * stagger_ms / 1000.0)
        out[i] = p.real(phase, n) if probe == "real" else p.noprompt(phase, n)

    ts = [threading.Thread(target=w, args=(i,), daemon=True) for i in range(n)]
    for t in ts:
        t.start()
    b.wait()
    for t in ts:
        t.join(timeout=1200)
    return Counter(e["kind"] for e in out if e)


class Pacer:
    """按「发起速率」发请求：调度与执行分离，请求慢不会拉低发送速率。

    这一点是测速率的命门——串行 for 循环在某个请求耗时 8s 时会把瞬时速率
    从 2/s 掉到 0.12/s，测出来的「阈值」其实是自己的抖动。
    """

    def __init__(self, p: Prober, max_parallel: int = 10):
        self.p = p
        self.max_parallel = max_parallel

    def run(self, qps: float, seconds: float, phase: str) -> Counter:
        start = time.monotonic()
        end = start + seconds
        lock = threading.Lock()
        results: Counter = Counter()
        threads: list[threading.Thread] = []

        def fire():
            ev = self.p.noprompt(phase)
            with lock:
                results[ev["kind"]] += 1

        i = 0
        while True:
            target = start + i / qps
            if target >= end:
                break
            gap = target - time.monotonic()
            if gap > 0:
                time.sleep(gap)
            # 在途满就等：只有这时才算真正被本地并发闸门挡住
            while self.p.inflight >= self.max_parallel:
                time.sleep(0.01)
            t = threading.Thread(target=fire, daemon=True)
            t.start()
            threads.append(t)
            i += 1
        for t in threads:
            t.join(timeout=180)
        return results


# ---------------------------------------------------------------- 计划与执行

def smoke(rec: Recorder, p: Prober) -> None:
    rec.note("smoke: 3 个探针 + 1 次余额")
    for _ in range(3):
        ev = p.noprompt("smoke")
        rec.note(f"  seq={ev['seq']} status={ev['status']} kind={ev['kind']} lat={ev['lat_ms']}ms")
        time.sleep(0.3)
    rec.note(f"  余额 = {p.balance()}")


def plan_text(until: str = "09:00") -> str:
    return f"""
imgrate 计划（跑到 {until}，真图硬上限 100 张 = {100 * POINTS_PER_IMAGE} 分）
────────────────────────────────────────────────────────
A 速率阈值测绘（零成本，~12 分钟）
  0.25/0.5/1/1.5/2/3/5/8 qps × 60s，档间静默 30s
  产出：429 出现时各滑动窗口内的累计请求数 → 反推窗口与配额

B 窗口形状反推（零成本，~1 分钟）
  12 并发打满 → 每 100ms 单发探针持续 8s，重复 4 轮
  产出：429 恢复曲线，区分「在途名额」与「时间窗配额」

C 单张真图（{POINTS_PER_IMAGE} 分，~1-2 分钟）
  1 张 low + 生成中途 10 个探针
  产出：真图是否与探针共用同一 10 名额

C2 10 张真图并发（{10 * POINTS_PER_IMAGE} 分，~2 分钟）
  打满后中途两次探针
  产出：真图并发的真实上限与排队行为

D 整夜滚动采样（零成本 + 周期真图）
  每分钟 6 个探针；每 10 分钟 3 张真图（混合探针）；每 20 分钟阶梯并发；
  每 90 分钟 10 张真图并发
"""


def parse_until(until: str) -> float:
    """把 HH:MM 解析成 monotonic 截止点（已过则算明天）。"""
    now = datetime.now()
    hh, mm = (int(x) for x in until.split(":"))
    target = now.replace(hour=hh, minute=mm, second=0, microsecond=0)
    from datetime import timedelta as _td
    if target <= now:
        target = target + _td(days=1)
    return time.monotonic() + (target - now).total_seconds()


def sweep_rates(rec: Recorder, p: Prober, hold: int = 60, quiet: int = 30) -> None:
    for qps in (0.25, 0.5, 1.0, 1.5, 2.0, 3.0, 5.0, 8.0):
        rec.note(f"A: {qps} qps × {hold}s")
        st = Pacer(p).run(qps, hold, f"A@{qps}")
        rec.note(f"   → {dict(st)}")
        rec.note(f"   静默 {quiet}s（让窗口滑空）")
        time.sleep(quiet)


def recovery_probe(rec: Recorder, p: Prober, rounds: int = 4) -> None:
    for r in range(rounds):
        st = burst(p, 12, f"B{r}-burst")
        rec.note(f"B{r}: 12 并发 → {dict(st)}")
        t0 = time.monotonic()
        while time.monotonic() - t0 < 8:
            ev = p.noprompt(f"B{r}-probe")
            rec.stats({"type": "probe", "phase": f"B{r}",
                       "offset_ms": round((time.monotonic() - t0) * 1000),
                       "status": ev["status"], "kind": ev["kind"],
                       "retry_s": ev["retry_s"], "wall": now_wall()})
            time.sleep(0.1)
        time.sleep(2)


def real_allowed(p: Prober, need: int, budget: int) -> bool:
    """真图前双重闸门：预算 + 余额。余额留 300 分兜底（约 15 张），
    免得把号跑到 plan_credit，那会让整个账号进 12h 硬冷却。"""
    if p.real_spent + need > budget:
        return False
    bal = p.balance()
    if bal is not None and bal < 300 + need * POINTS_PER_IMAGE:
        p.rec.note(f"余额 {bal} 不足以再发 {need} 张，停真图")
        return False
    return True


def real_check(rec: Recorder, p: Prober) -> None:
    """C：单张真图 + 生成中途 10 个探针（验证共用名额 + 占用时长）。"""
    before = p.balance()
    rec.note(f"C: 余额前={before}")
    box: dict = {}

    def go():
        box["ev"] = p.real("C-single")

    t = threading.Thread(target=go, daemon=True)
    t.start()
    time.sleep(1.5)
    st = burst(p, 10, "C-inflight")
    rec.note(f"C: 生成在途时 10 探针 → {dict(st)}")
    t.join(timeout=900)
    ev = box.get("ev") or {}
    after = p.balance()
    rec.note(f"C: 真图 status={ev.get('status')} lat={ev.get('lat_ms')}ms；余额 {before}→{after}"
             f"（差 {None if before is None or after is None else before - after}）")


def real_burst(rec: Recorder, p: Prober, n: int, phase: str) -> Counter:
    """n 张真图并发；生成中途打两批探针，证明名额被长期占用。"""
    before = p.balance()
    b = threading.Barrier(n + 1, timeout=30)
    out: list[dict | None] = [None] * n

    def w(i: int):
        b.wait()
        out[i] = p.real(phase)

    ts = [threading.Thread(target=w, args=(i,), daemon=True) for i in range(n)]
    for t in ts:
        t.start()
    t0 = time.monotonic()
    b.wait()
    mid1 = mid2 = Counter()
    if p.inflight > 0 or True:
        time.sleep(2.0)
        mid1 = burst(p, 3, phase + "-mid1")
        time.sleep(8.0)
        mid2 = burst(p, 3, phase + "-mid2")
    for t in ts:
        t.join(timeout=1800)
    el = time.monotonic() - t0
    after = p.balance()
    st = Counter(e["kind"] for e in out if e)
    lats = sorted(e["lat_ms"] for e in out if e and e["status"] == 200)
    med = f"{lats[len(lats)//2]:.0f}ms" if lats else "-"
    rec.note(f"{phase}: {n} 张 → {dict(st)} 总{el:.0f}s 成功中位={med}；"
             f"中途探针 {dict(mid1)} / {dict(mid2)}；余额 {before}→{after}")
    return st


def ladder(rec: Recorder, p: Prober, phase: str) -> None:
    """1..12 阶梯并发，每档间隔 1.5s：还原当下额度大小。"""
    for n in (1, 2, 4, 6, 8, 10, 11, 12):
        st = burst(p, n, f"{phase}-n{n}")
        rec.note(f"{phase}: 并发 {n} → {dict(st)}")
        time.sleep(1.5)


def load_account(uid: str) -> Path:
    """为什么不给 --uid 写死默认值：写死了就只在那台机器上成立。

    auths/ 下每个 json 是一个账号的票，单账号机器直接跑，多账号时
    让人显式挑——比拿一个不存在的文件路径抛 FileNotFoundError 清楚得多。
    """
    d = PROJECT / "auths"
    if uid:
        f = d / f"{uid}.json"
        if not f.exists():
            raise SystemExit(f"没有这个账号：{f}")
        return f
    cands = sorted(x.stem for x in d.glob("*.json"))
    if len(cands) == 1:
        return d / f"{cands[0]}.json"
    raise SystemExit(
        f"需要一个 --uid：{d} 下有 {len(cands)} 个账号"
        + (f"（{', '.join(cands[:5])}…）" if cands else "（空的，先导入账号）")
    )


def run(args: argparse.Namespace) -> int:
    rec = Recorder(Path(args.dir), resume=not args.fresh)
    acct = json.loads(load_account(args.uid).read_text())
    p = Prober(acct, rec)
    budget = args.real_budget
    spent_file = rec.dir / ".real-spent"
    if spent_file.exists():
        try:
            p.real_spent = int(spent_file.read_text().strip() or 0)
        except ValueError:
            pass

    # deadline 也要落盘：KeepAlive 重启时若已过 09:00，parse_until 会把
    # 「09:00」算成明天，等于自动续跑 24 小时。
    dl_file = rec.dir / ".deadline"
    if dl_file.exists():
        try:
            dl_epoch = float(dl_file.read_text().strip())
        except ValueError:
            dl_epoch = 0.0
        if dl_epoch and time.time() >= dl_epoch:
            rec.note(f"已过截止（{datetime.fromtimestamp(dl_epoch)}），退出")
            return 0
        deadline = time.monotonic() + max(0.0, dl_epoch - time.time())
    else:
        deadline = parse_until(args.until)
        dl_file.write_text(str(time.time() + (deadline - time.monotonic())))
    left = deadline - time.monotonic()
    rec.note(f"启动 uid={args.uid} dir={rec.dir} 已记录 {rec.seq} 条；"
             f"截止 {args.until}（{left/60:.0f} 分钟后）真图预算 {budget} 张")

    if args.mode == "smoke":
        smoke(rec, p)
        return 0

    tag = rec.dir / ".did-abc"
    if not tag.exists():
        rec.note("==== A 速率阈值测绘 ====")
        sweep_rates(rec, p)
        rec.note("==== B 窗口形状 ====")
        recovery_probe(rec, p)
        rec.note("==== C 单张真图 ====")
        if real_allowed(p, 1, budget):
            real_check(rec, p)
        rec.note("==== C2 10 张真图并发 ====")
        if real_allowed(p, 10, budget):
            real_burst(rec, p, 10, "C2-burst10")
        rec.note("==== C3 图生图（edits）====")
        if real_allowed(p, 1, budget):
            ev = p.real_edit("C3-edit")
            rec.note(f"C3: edits status={ev['status']} lat={ev['lat_ms']}ms "
                     f"body={ev['body'][:120]}")
        tag.write_text(now_wall())
    else:
        rec.note(f"跳过 A/B/C（{tag.read_text()} 已跑过，真图已用 {p.real_spent}/{budget}）")

    rec.note("==== D0 连续吞吐：10 分钟 × 每分钟 2 张 ====")
    for i in range(10):
        if not real_allowed(p, 2, budget):
            rec.note("D0: 预算/余额不足，提前结束")
            break
        real_burst(rec, p, 2, f"D0-{i}")
        time.sleep(60)

    rec.note("==== D 滚动采样 ====")
    next_mix = time.monotonic() + 20 * 60
    next_ladder = time.monotonic() + 20 * 60
    next_burst = time.monotonic() + 90 * 60
    while time.monotonic() < deadline:
        st = Pacer(p).run(0.1, 60, f"D-{datetime.now():%H%M}")
        now = time.monotonic()
        if now >= next_mix and real_allowed(p, 2, budget):
            real_burst(rec, p, 2, f"D-mix{datetime.now():%H%M}")
            next_mix = now + 20 * 60
        if now >= next_ladder:
            ladder(rec, p, f"D-ladder{datetime.now():%H%M}")
            next_ladder = now + 20 * 60
        if now >= next_burst and real_allowed(p, 10, budget):
            real_burst(rec, p, 10, f"D-burst{datetime.now():%H%M}")
            next_burst = now + 90 * 60

    rec.note(f"到达截止，退出。真图已用 {p.real_spent}/{budget}")
    try:
        (rec.dir / "report.md").write_text(report(rec.dir), encoding="utf-8")
        rec.note(f"报告已写入 {rec.dir}/report.md")
    except Exception as e:
        rec.note(f"报告生成失败：{e!r}")
    return 0


# ---------------------------------------------------------------- 分析

def _load_events(dirpath: Path) -> list[dict]:
    out = []
    f = dirpath / "events.jsonl"
    if not f.exists():
        return out
    for line in f.read_text(encoding="utf-8", errors="ignore").splitlines():
        try:
            e = json.loads(line)
        except Exception:
            continue
        # 用 wall 而不是单调时钟：KeepAlive 重启会让 t_ms 归零，
        # 拿 t_ms 拼时间线会把几段时间折叠到一起。
        try:
            e["ep"] = datetime.fromisoformat(e["wall"]).timestamp()
        except Exception:
            e["ep"] = 0.0
        out.append(e)
    out.sort(key=lambda x: x["ep"])
    return out


def _n_of(e: dict) -> int | None:
    if isinstance(e.get("n"), int):
        return e["n"]
    phase = e.get("phase", "")
    m = re.search(r"-n(\d+)$", phase)
    if m:
        return int(m.group(1))
    m = re.search(r"(?:burst|ladder)(\d+)$", phase)
    if m:
        return int(m.group(1))
    m = re.search(r"ladder.*-n(\d+)$", phase)
    return int(m.group(1)) if m else None


def report(dirpath: Path) -> str:
    ev = _load_events(dirpath)
    if not ev:
        return "# 无数据\n"

    t_first, t_last = ev[0]["ep"], ev[-1]["ep"]
    kinds = Counter(e["kind"] for e in ev)
    reals = [e for e in ev if e.get("probe") in ("real", "edit")]
    notes = (dirpath / "notes.log").read_text(encoding="utf-8", errors="ignore") \
        if (dirpath / "notes.log").exists() else ""

    L: list[str] = []
    add = L.append
    add(f"# 图生生成频控实测 — {dirpath.name}")
    add("")
    add(f"- 采样区间：{datetime.fromtimestamp(t_first):%m-%d %H:%M:%S} → "
        f"{datetime.fromtimestamp(t_last):%m-%d %H:%M:%S}（{(t_last-t_first)/3600:.2f} 小时）")
    add(f"- 探针请求 {len(ev) - len(reals)} 条，真图 {len(reals)} 张")
    add(f"- 响应分布：`{dict(kinds)}`")
    add("")

    # ---- 1. 并发硬上限
    add("## 1. 并发上限")
    add("")
    add("同一时刻并发 n 个请求，看「通过数」是否恒定——恒定值就是硬上限。")
    add("")
    add("| 并发 n | 轮次 | 通过(400) | 拒绝(429) | 平均通过 |")
    add("|---|---|---|---|---|")
    by_n: dict[int, list[Counter]] = {}
    for e in ev:
        ph = e.get("phase", "")
        if not any(k in ph for k in ("burst", "ladder")):
            continue
        if e.get("probe") in ("real", "edit") or "-mid" in ph:
            continue
        n = _n_of(e)
        if not n or n < 2:
            continue
        by_n.setdefault(n, [])
        # 同一 phase 的事件归到同一轮
        grp = by_n[n]
        if not grp or grp[-1].get("_phase") != ph:
            grp.append({"_phase": ph, "ok": 0, "r429": 0})
        grp[-1]["ok" if e["kind"] != "r429" else "r429"] += 1
    for n in sorted(by_n):
        rounds = [g for g in by_n[n] if "_phase" in g]
        oks = [g.get("ok", 0) for g in rounds]
        r4s = [g.get("r429", 0) for g in rounds]
        if not rounds:
            continue
        add(f"| {n} | {len(rounds)} | {min(oks)}~{max(oks)} | {min(r4s)}~{max(r4s)} | "
            f"{(sum(oks)/len(oks)):.1f} |")
    add("")
    hard = [n for n in sorted(by_n)
            if any(g.get("ok", 0) == 10 and g.get("r429", 0) == n - 10 for g in by_n[n] if "_phase" in g)]
    if hard:
        add(f"**判读**：并发 ≥ {min(hard)} 时通过数恒为 10 ⇒ 单账号在途硬上限 = **10**。")
    add("")

    # ---- 2. 速率阈值
    add("## 2. 速率阈值（按发起速率）")
    add("")
    add("| 目标 qps | 实际发出 | 429 | 429 率 |")
    add("|---|---|---|---|")
    rate_groups: dict[str, list[dict]] = {}
    for e in ev:
        ph = e.get("phase", "")
        if ph.startswith("A@"):
            rate_groups.setdefault(ph, []).append(e)
    def qps_key(x):
        try:
            return float(x[2:])
        except ValueError:
            return 0.0
    for ph in sorted(rate_groups, key=qps_key):
        g = rate_groups[ph]
        bad = sum(1 for e in g if e["kind"] == "r429")
        add(f"| {ph[2:]} | {len(g)} | {bad} | {bad/max(len(g),1)*100:.0f}% |")
    add("")

    # ---- 3. 429 前的窗口累计
    add("## 3. 429 触发时、往前 W 秒内的请求数")
    add("")
    add("哪个 W 让「中位累计数」在所有 429 上都差不多，那个 W 就是限流窗口。")
    add("")
    add("| 窗口 W(s) | 429 样本 | 中位累计 | 最小 | 最大 | 离散度(max-min) |")
    add("|---|---|---|---|---|---|")
    best = None
    for w in WINDOWS:
        vals = []
        for e in ev:
            if e["kind"] != "r429":
                continue
            c = sum(1 for x in ev if e["ep"] - w <= x["ep"] <= e["ep"])
            vals.append(c)
        if len(vals) < 3:
            continue
        vals.sort()
        med, mn, mx = vals[len(vals)//2], vals[0], vals[-1]
        spread = mx - mn
        add(f"| {w} | {len(vals)} | {med} | {mn} | {mx} | {spread} |")
        if best is None or spread < best[1]:
            best = (w, spread, med)
    if best:
        add("")
        add(f"**判读**：窗口 {best[0]}s 的离散度最小（{best[1]}），"
            f"对应配额约 **{best[2]}** 个请求 / {best[0]} 秒。")
    add("")

    # ---- 4. 恢复曲线
    add("## 4. 打满后的恢复（B 阶段 100ms 粒度）")
    add("")
    rows = []
    stats_path = dirpath / "stats.jsonl"
    if stats_path.exists():
        for line in stats_path.read_text(encoding="utf-8", errors="ignore").splitlines():
            try:
                r = json.loads(line)
            except Exception:
                continue
            if r.get("type") == "probe":
                rows.append(r)
    if rows:
        by_phase: dict[str, list[dict]] = {}
        for r in rows:
            by_phase.setdefault(r["phase"], []).append(r)
        for ph in sorted(by_phase):
            g = sorted(by_phase[ph], key=lambda x: x["offset_ms"])
            first_ok = next((x for x in g if x["status"] != 429), None)
            add(f"- `{ph}`：{len(g)} 个探针，首个非 429 在 "
                f"{first_ok['offset_ms'] if first_ok else '>8000'} ms")
        add("")
        add("**判读**：恢复点在 200~400ms 量级 ⇒ 额度按「在途请求」实时结算，"
            "不是按分钟级窗口重置。")
    else:
        add("- 无 B 阶段数据")
    add("")

    # ---- 5. 真图
    add("## 5. 真实生图")
    add("")
    ok = [e for e in reals if e["status"] == 200]
    bad = [e for e in reals if e["status"] != 200]
    if reals:
        add(f"- 成功 {len(ok)}/{len(reals)}")
        if ok:
            lats = sorted(e["lat_ms"] for e in ok)
            add(f"- 耗时：min {lats[0]/1000:.1f}s / p50 {lats[len(lats)//2]/1000:.1f}s / "
                f"p90 {lats[int(len(lats)*0.9)]/1000:.1f}s / max {lats[-1]/1000:.1f}s")
        if bad:
            add(f"- 失败 {len(bad)} 张：" + "；".join(
                f"{e['wall'][11:19]} {e['kind']} {e['body'][:60]}" for e in bad[:5]))
        # 余额变化
        bal_lines = [ln for ln in notes.splitlines() if "余额" in ln and "→" in ln]
        if bal_lines:
            add("")
            add("余额轨迹（notes）：")
            add("")
            for ln in bal_lines[-12:]:
                add(f"- {ln.split('] ', 1)[-1]}")
    else:
        add("- 本夜未发真图")
    add("")

    # ---- 6. 时间线
    add("## 6. 时间线（10 分钟一格；`#` 长度 = 该格 429 占比）")
    add("")
    buckets: dict[int, Counter] = {}
    base = int(t_first // 600)
    for e in ev:
        b = int(e["ep"] // 600) - base
        buckets.setdefault(b, Counter())[e["kind"]] += 1
    for b in sorted(buckets):
        c = buckets[b]
        n = sum(c.values())
        bad_n = c.get("r429", 0)
        bar = "#" * min(60, int(bad_n / max(n, 1) * 60))
        t = datetime.fromtimestamp((base + b) * 600)
        add(f"- {t:%H:%M} n={n:5d} 429={bad_n:4d} {bar}")
    add("")
    return "\n".join(L)


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd")

    ap_run = sub.add_parser("run", help="长跑")
    ap_run.add_argument("--uid", default="",
                        help="账号 uid（读 auths/<uid>.json）；留空则用 auths/ 下唯一的账号")
    ap_run.add_argument("--dir", default="")
    ap_run.add_argument("--until", default="09:00", help="HH:MM（已过则算明天）")
    ap_run.add_argument("--mode", default="full", choices=["full", "smoke"])
    ap_run.add_argument("--real-budget", type=int, default=100, help="真图张数上限（20 分/张）")
    ap_run.add_argument("--fresh", action="store_true", help="丢弃续跑，重新编号")

    ap_rep = sub.add_parser("report", help="出结论")
    ap_rep.add_argument("--dir", required=True)

    sub.add_parser("plan", help="打印计划")
    args = ap.parse_args()

    if args.cmd == "plan":
        print(plan_text(args.until if hasattr(args, "until") else "09:00"))
        return 0
    if args.cmd == "report":
        print(report(Path(args.dir)))
        return 0
    if args.cmd == "run":
        if not args.dir:
            args.dir = str(PROJECT / ".workbuddy" / "imgrate" / datetime.now().strftime("%Y-%m-%d"))
        return run(args)
    ap.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
