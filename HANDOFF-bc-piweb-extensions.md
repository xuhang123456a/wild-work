# 决议梳理：BC 压缩内核 + pi-web 扩展套件（交付实施版）

> 生成于 2026-10-24，来源：pi-web 内 BC 部署与扩展路线的完整对话。
> 所有版本/路径/API 均为本机实证，非推断。实施前先读「红线」一节。

## 0. 背景一句话

以 **billion-context（BC, acp-kernel）为压缩内核**、以 **pi-web 现有扩展机制为唯一集成层**，
构建「bc 压缩 + 全局 FTS 搜索 + 会话知识库 + 看板娘入口 + 语音 I/O + wild-work 内置 provider」六件套；
**不 fork pi-web**（除非未来确需），**不 fork pi**。

## 1. 环境现状（已验证，勿重复取证）

- pi-web **0.12.2**（绑定 pi-coding-agent **1.2.0**，node>=22.19）；上游 pi 已 1.4.0 但 pi-web 精确钉死 1.2.0；扩展事件 41 个在 1.2.0/1.4.0 **完全一致**。
- BC **0.1.202** 已以官方机制运行：`bili plugin install pi` → `C:/Users/rocks/.pi/agent/settings.json` 的 `packages += "npm:billion-context"`（解析到 `D:/data/nvm/npm-global/node_modules/billion-context`，无二次下载）；BC 自动备份 `settings.json.bili-bak`。
- 配置归位：`C:/Users/rocks/.config/billion-context/billion-context.json` = `{"providers":{},"autoUpdate":false,"pi":{"subagents":false}}`（关自动更新防 EBUSY 文件锁；关 acp_delegate 防与 pi-web 内置 subagent 双工具面冲突；废弃的 `~/.pi/acp.json` 已删）。
- pi 原生自动压缩已关：settings.json `"compaction": {"enabled": false}`（**老会话未生效、新会话生效**；重启 pi-web 才全局生效——pi-web 每次创建 agent 才重读 settings，无 fs.watch）。
- BC 代理：扩展自动拉起 `node .../billion-context/dist/index.js start --port 18787`，看门狗托管；仪表盘 `http://127.0.0.1:18787/__bili/`。
- pi-web 进程：launcher PID 32964 + next 30141（只绑 LAN IP 10.126.126.2，**curl 127.0.0.1:30141 不通**）。
- BC 窗口分母虚高问题已知：pi-web lane 不上报窗口头，须手配 `providers["http://10.127.127.10:7863"].models["workbuddy/hy3"].compress.modelContextLimit=180000`（headroom：hy3 maxTokens 64000 × 默认 0.25 ⇒ 有效 135000）——**用户未拍板，挂起**。
- 本会话 37 请求账本：input 4.54M / cached 86.2% / miss 拆账 = 新内容 152.8K + 压缩重付 85.5K + **TTL/relay 386.6K（62%，miss 大头是空闲过期不是压缩）**；折叠净收益 +347K。

## 2. 决议清单

### A. BC 集成（方案 B：扩展层全权旁路，非代理模式）
1. 压缩器不 fork pi 内核，纯扩展 API 实现：`context_with_system` 每轮跑 BC 管线投影 + `session_before_compact` 返回 `{cancel:true}`（仅 reason=threshold/overflow）+ `registerTool` 注册 compress/decompress/search_context/acp_status + `sessionManager.appendCustomEntry("acp-state", state)` 落盘块树（append-only jsonl 与「折叠不删原文」同构，fork/branch/tree 自动带状态）。
2. **必须逐字保留的内核资产**：kernel 14,024 字符哲学文本（`kernel/src/compression-rules.ts`）；管线顺序 assign-refs→sync-blocks→merge-blocks→prune→ccr-store→filter→hide-compress-calls→nudge-inject→emergency-truncate→render-refs；门控常量（占用带 0.45–0.55 / 强制 0.75 / 截断 0.95 / 增速下限 ~22.5K / 步长 50K / tail-bias 引导保前缀缓存）；#4348 压缩回执只保最近 3 个；turn-integrity/#3953 边界不挖洞；CCR 三纪律（replace-once-at-arrival、占位带 ref、原文低信任层）。
3. 先 shadow 模式（只记账不接管），观测一致后切接管。
4. BC 官方代理模式与扩展模式**不混用**：切扩展模式后停 18787 代理或保持其仅服务其它宿主。

### B. 扩展套件（四个 npm 扩展 + 一个皮肤包）
5. 统一形态：monorepo 四小包 + 共享 `pi-ext-kit`；**同一份 ExtensionFactory 代码 npm/内置两种分发零改动互换**，fork 决策无限期推迟。
6. ① `ww-provider`：registerProvider 注册 wild-work（模型目录/窗口元数据），最简单，**首个落地趟通加载链路**。
7. ② `session-kb`：读 `~/.pi/agent/sessions/**.jsonl` 建 sqlite-fts5 索引；`registerCommand("/fts")` + `registerTool("kb_query")`；**HTTP-first**——内嵌 localhost HTTP 服务（固定高位端口，如 32178，只绑 127.0.0.1 + CORS 允许 userscript 跨源），userscript 与 registerTool 是平级消费者。与 BC 块树互补：BC 管会话内召回，KB 管跨会话沉淀。
8. ③ `tts-out`：`after_provider_response` → OS TTS（Windows sapi 或 wild-work 侧 TTS 端点）；registerCommand 开关。
9. ④ `bili-compressor`：即 A 组方案 B 实体。
10. ⑤ 看板娘皮肤包（Tampermonkey userscript）：**纯皮肤无逻辑**——桌宠渲染+气泡+搜索窗+录音按钮，`GM_xmlhttpRequest` 直连 ② 的 localhost API；搜索结果跳转调 pi-web SPA 路由落到对应会话；扩展经 WS 推事件（agent_end/折叠完成）驱动主动开口（气泡+TTS）。
11. STT：用户在浏览器侧 Web Speech → userscript → ② 的 HTTP 转写端点（wild-work 可加 Whisper 路由）。
12. pi-web 内置 `app/api/sessions/search` 先评估够不够，② 做增强不做重复。

### C. 上游贡献策略（PR 定位=顺手贡献，非功能入口）
13. 双上游分工：`earendil-works/pi`（内核/扩展 API/compaction）vs `agegr/pi-web`（渲染层）。功能全部走扩展层，不提功能 PR。
14. pi-web CONTRIBUTING 明确 out-of-scope（写得好也拒）：compaction 类改动、可做成扩展的功能（点名 new providers）、pi-web-only 扩展 API、看板娘/皮肤类视觉定制、新子系统。**扩展路径长期有效是项目核心承诺**（"Extensions written for the CLI work in Pi Web unchanged"）。
15. 易合并区（维护者自列）：扩展 UI 渲染改进（setWidget/custom/对话框——做 B 组时必然撞到，顺手修）、Windows/PWA/移动端修复（用户主环境）、性能（带 acp_cache 式前后测量）、i18n（en/zh-CN/zh-TW 三份齐）。
16. PR 纪律：feature 先开 issue、一 PR 一事、`npm test`+`tsc --noEmit`+`lint` 三绿、不加运行时依赖、先读 `docs/agents/` 对应笔记。
17. 只有发现**扩展 API 本身缺口**时才去 `earendil-works/pi` 提 issue/PR——唯一「碰 pi」场景。

### D. 暂缓/挂起项（勿擅自实施）
18. BC 升级 0.1.204：用户明确暂缓（含 #4090 超窗保险丝等收益，升级时先停 pi-web 再 npm -g）。
19. BC `compress.modelContextLimit=180000` + pi models.json contextWindow 对齐：待用户拍板。
20. `showCacheMissNotices:false`（缓存 miss 文案是 pi 自己的 5 分钟启发式，走 wild-work 渠道语义不准）：待用户拍板。
21. 重启 pi-web 使 compaction:false 全局生效：待用户择机（重启会中断当前会话）。

## 3. 实施顺序

```
第 0 步  ww-provider 空壳工厂 → npm 包 → pi-web packages 安装 → 模型出现在选择器（趟通链路）
第 1 步  session-kb：HTTP-first 服务 + fts 索引 + kb_query 工具（含 /fts 命令）
第 2 步  tts-out
第 3 步  皮肤包 v1：娘 + 搜索窗接 ① 的 HTTP 面
第 4 步  bili-compressor：shadow 模式（读 BC kernel 源码 ref/billion-context/kernel/）
第 5 步  bili-compressor 切接管；皮肤包 v2（压缩状态气泡/主动提醒）
```

## 4. 红线

- **不 fork pi-web / 不 fork pi**，一切走官方扩展机制；唯一例外见决议 17。
- **不动 pi 会话文件格式**（只 appendCustomEntry，不改写已有 jsonl）。
- **BC kernel 哲学文本与管线顺序逐字保留**，不得「优化」。
- 不对上游穷举探测（wild-work AGENTS §0.1 纪律同样适用于 pi/BC 上游验证）。
- 修改 pi-web 全局配置前先备份（settings.json / billion-context.json 均有 .bak 先例）。
- userscript 只做皮肤，业务逻辑一律住扩展（防 DOM 碎导致功能死）。

## 5. 关键路径速查

| 项 | 值 |
|---|---|
| pi 扩展 API 类型 | `@earendil-works/pi-coding-agent/dist/core/extensions/types.d.ts`（1.2.0：registerTool/registerCommand/registerProvider/registerVirtualModel/showImages/context/context_with_system/session_before_compact/after_provider_response 等 41 事件） |
| pi-web 内置扩展先例 | `ref/pi-web-src/lib/builtin-extensions.ts:229`（BUILTIN_EXTENSION_NAMES）、`lib/rpc-manager.ts:2610`（extensionFactories 数组） |
| UI 通道 | `lib/types.ts:139-211` ExtensionUiRequest（select/confirm/input/editor/custom/setWidget/setStatus/notify/registerCommand…）；widget=文本行 AnsiText，无图片通道 |
| BC kernel 源码 | `ref/billion-context/kernel/src/`（纯函数零依赖，compression-rules.ts 14,024 字符、crush.ts 1832 行、compress.ts 1826 行） |
| pi 原生压缩对照 | `ref/pi-src/packages/coding-agent/src/core/compaction/`（1,654 行，破坏式无召回——替换是纯升级） |
| 会话文件 | `~/.pi/agent/sessions/**.jsonl`；pi-web 仪表盘 `http://10.126.126.2:30141`；BC 仪表盘 `http://127.0.0.1:18787/__bili/` |
| 参考仓库 | `ref/billion-context`、`ref/pi-src`、`ref/pi-web-src`（均已克隆在 D:/work/wild-work/dev/ref/） |
