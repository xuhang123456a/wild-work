#!/usr/bin/env python3
# bisect-blocked.py — 对 data/blocked/ 里的被拦请求体做分层重放与二分定位。
#
# 用法：
#   python3 scripts/bisect-blocked.py <blocked-dir> \
#       [--file blocked-XXX.json] [--base-url http://127.0.0.1:7863/v1] \
#       [--key KEY | --key-file PATH] [--model M] [--max-requests N]
#
# 只读只重放，不改任何文件；被拦样本来自 DumpBlocked 落盘（不含凭据头）。
# 纪律（AGENTS.md §0.1）：每次重放=一次真实上游请求，默认 --max-requests 24，
# 打满即停；请用小号渠道跑，禁止循环探测。
#
# 流程（与《DSH兼容-非法客户端拦截定位与修复.md》第七章一致）：
#   1) 整包重放           → 确认仍被拦（200 则说明已修复/指纹失效，直接结束）
#   2) 只留首条 system    → 判断指纹是否在 system 开头
#   3) system 按句前缀二分 → 找到最短被拦前缀
#   4) 单句/相邻两句重放   → 区分单句指纹与跨句组合指纹
#   5) 打印元凶句与修复建议（对照 sanitize 表改写目标）

import argparse
import glob
import json
import os
import re
import sys
import urllib.error
import urllib.request

CHAT_PATH = "/chat/completions"
SENT_SPLIT = re.compile(r"(?<=[.!?。！？])\s+")


def http_post(url: str, key: str, body: bytes, timeout: float = 60.0):
    req = urllib.request.Request(
        url, data=body, method="POST",
        headers={
            "Content-Type": "application/json",
            "Authorization": f"Bearer {key}",
        },
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def is_blocked(status: int, text: str) -> bool:
    if status == 200:
        return False
    return "11128" in text or "Illegal API invocation" in text or "unapproved channel" in text


def verdict(status: int, text: str) -> str:
    if status == 200:
        return "OK(200)"
    if is_blocked(status, text):
        return f"BLOCKED({status} 11128)"
    return f"OTHER({status}: {text[:120]!r})"


def system_only(obj: dict):
    msgs = obj.get("messages") or []
    for m in msgs:
        if m.get("role") in ("system", "developer"):
            return [m, {"role": "user", "content": "hi"}]
    return None


def sentences(text: str):
    return SENT_SPLIT.split(text.strip())


def replay(tag: str, obj: dict, args, budget: dict) -> str:
    if budget["left"] <= 0:
        return "SKIP(max-requests)"
    budget["left"] -= 1
    payload = dict(obj)
    payload["messages"] = obj["messages"]
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    status, text = http_post(args.base_url.rstrip("/") + CHAT_PATH, args.key, body)
    v = verdict(status, text)
    print(f"  [{tag}] {v}")
    return v


def main() -> int:
    ap = argparse.ArgumentParser(description="bisect a blocked request body (read-only replay)")
    ap.add_argument("blocked_dir", help="data/blocked 目录（DumpBlocked 落盘处）")
    ap.add_argument("--file", help="指定样本文件名（默认取最新一份）")
    ap.add_argument("--base-url", default=os.environ.get("WILDWORK_BASE_URL", "http://127.0.0.1:7863/v1"))
    ap.add_argument("--key", default=os.environ.get("WILDWORK_API_KEY"))
    ap.add_argument("--key-file", help="从文件读 API key（如部署目录 .apikey）")
    ap.add_argument("--model", help="覆盖 model 字段")
    ap.add_argument("--max-requests", type=int, default=24)
    args = ap.parse_args()

    if not args.key:
        if args.key_file and os.path.isfile(args.key_file):
            args.key = open(args.key_file, encoding="utf-8").read().strip()
        elif os.path.isfile("data/.apikey"):
            args.key = open("data/.apikey", encoding="utf-8").read().strip()
    if not args.key:
        sys.exit("缺少 API key：用 --key / --key-file / 环境变量 WILDWORK_API_KEY")

    pattern = os.path.join(args.blocked_dir, "blocked-*.json")
    if args.file:
        path = os.path.join(args.blocked_dir, args.file)
    else:
        matches = sorted(glob.glob(pattern))
        if not matches:
            sys.exit(f"{pattern} 下没有样本（DumpBlocked 未落盘或已被清）")
        path = matches[-1]  # 文件名含 unixnano，字典序即时间序
    print(f"样本: {path}")
    obj = json.load(open(path, encoding="utf-8"))
    if args.model:
        obj["model"] = args.model
    if not obj.get("messages"):
        sys.exit("样本无 messages")

    budget = {"left": args.max_requests}
    print(f"请求预算: {args.max_requests}（超出即停，§0.1 最小请求数）")

    # 1) 整包重放
    budget["left"] -= 1
    status, text = http_post(args.base_url.rstrip("/") + CHAT_PATH, args.key,
                             open(path, "rb").read())
    print(f"整包: {verdict(status, text)}")
    if not is_blocked(status, text):
        print("已放行（版本已修复或指纹失效），无需二分")
        return 0

    # 2) 只留首条 system
    sysmsg = system_only(obj)
    if sysmsg is None:
        print("无 system 消息：指纹不在 system（检查 messages 其它字段）")
        return 0
    so = dict(obj)
    so["messages"] = sysmsg
    v = replay("system-only", so, args, budget)
    if v != "SKIP(max-requests)" and not v.startswith("BLOCKED"):
        print("system-only 放行：指纹在后续消息或非 system 字段，本脚本止步于此")
        return 0

    text = sysmsg[0].get("content")
    if isinstance(text, list):  # 多模态取首个 text part
        text = next((p.get("text", "") for p in text if isinstance(p, dict) and p.get("type") == "text"), "")
    if not isinstance(text, str) or not text.strip():
        print("system content 非 string/无 text part：止步")
        return 0

    # 3) 前缀二分（最短被拦前缀）
    sents = sentences(text)
    culprit_idx = None
    lo, hi = 1, len(sents)
    # 先线性找第一个被拦前缀（句数少，线性即可，省请求）
    for i in range(1, len(sents) + 1):
        pre = dict(obj)
        pre["messages"] = [{"role": sysmsg[0]["role"], "content": " ".join(sents[:i])},
                           {"role": "user", "content": "hi"}]
        v = replay(f"prefix×{i}", pre, args, budget)
        if v.startswith("BLOCKED"):
            culprit_idx = i - 1
            break
        if v == "SKIP(max-requests)":
            break
    if culprit_idx is None:
        print("system 全句前缀均放行：指纹可能是整段连续逐字串（跨句组合），或 budget 打满")
        return 0
    print(f"\n元凶句（第 {culprit_idx + 1} 句触发拦截）:")
    print(f"  {sents[culprit_idx]!r}")

    # 4) 单句 vs 跨句组合
    one = dict(obj)
    one["messages"] = [{"role": sysmsg[0]["role"], "content": sents[culprit_idx]},
                       {"role": "user", "content": "hi"}]
    v1 = replay("single", one, args, budget)
    if v1.startswith("BLOCKED"):
        print("→ 单句指纹：把该句按 sanitize 表改写目标做最小扰动即可")
    elif culprit_idx > 0 and budget["left"] > 0:
        combo = dict(obj)
        combo["messages"] = [{"role": sysmsg[0]["role"],
                              "content": " ".join(sents[culprit_idx - 1:culprit_idx + 1])},
                             {"role": "user", "content": "hi"}]
        v2 = replay("pair", combo, args, budget)
        if v2.startswith("BLOCKED"):
            print("→ 跨句组合指纹：单句放行、与上一句连写才拦（如 @ 路径跨句指纹）")
        else:
            print("→ 组合不止两句：扩大窗口重放（受 budget 限制，本脚本止步）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
