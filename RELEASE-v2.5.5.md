# wild-work v2.5.5

> 发布主题：**社区 3 个 PR 入库 + 一次性修复流式截断伪装成功（issue #42）、TraeWork 登录首因掩盖（issue #50）、WorkBuddyAI 模型级限流拖垮整账号（issue #53）**。
> 本版共：3 个已合 PR（来自 xxhhlk）+ 4 项变更（截断修复 / 登录修复 / 模型级限流 / 流式超时推广），上次 tag v2.5.4 之后累积 6 个提交。

---

## 🔧 已入库的社区 PR（来自 xxhhlk）

| PR | 内容 |
| --- | --- |
| #51 | **qoder 系带 tool_calls 的 assistant 消息 content 为 null 致上游整请求拒答**：在服务器层 `rewriteModel` 规范工具轮空 content；实测矩阵 + 回归测试锁定。放对位置（它覆盖不了 PrepareBody 的渠道，且兼容响应层复用内层） |
| #54 | **去掉告别弹窗上的 `MB_DEFAULT_DESKTOP_ONLY`**：弹窗回到屏幕中央、不再半屏出界，且避免锁屏/RDP/UAC 下 MessageBox 永不返回 |
| #55 | **Win 托盘菜单图标用 PNG+ICO 容器 + 退出前摘除图标**：修掉上游自研 PNG 编码器坏（长度恒 0/CRC 错）导致的图标空白，退出不再留幽灵图标 |

## 🐛 本版新增修复

### issue #42 — 流式截断被伪装成「正常收尾」（[DONE]）

**现象**：Zed 等严格客户端偶发报 *tool input was not fully received*，原样重试往往就成功；参数越长/并行 tool_call 越多越容易触发；直连复现排除网络跳。

**实证**：qwenwork / qodercn / qodercom / qoder 四份 `sse.go` 的 `sawDone` 是**死变量**（声明后从不赋值）。于是：
- 上游连接中断 / 读超时被掐断，或断在半个帧上（末行无换行），出口照样补 `data: [DONE]`；
- 客户端看到「以 `[DONE]` 完美收尾」的流，只是 tool_call `arguments` 少了后半截 → 只能报「输入没收全」，而网关日志**没有任何错误**，极难排查。

**修法**：
1. `parseNestedSSE` 增加 `truncated` 出参（读错误 / 半帧 → true），`streamAsOpenAI` 里截断时改发一帧 OpenAI 规范 `error`（`code: upstream_truncated`）且**不发** [DONE]。
2. 通用流式路径（WorkBuddy/WorkBuddyAI/oczen 共用 `upstream/sse.go`）同样改造。
3. 刻意保留：上游「正常 EOF 但漏发 [DONE]」（帧完整、末行有换行）仍兜底补 [DONE]，避免误伤本就补 DONE 的渠道。
4. **流式超时推广到全渠道**（对齐 R35）：之前只有 glm / traework 有独立 `StreamHTTP`（无 `Client.Timeout`），其余 7 个渠道（qwenwork/qodercn/qodercom/qoder/workbuddy/workbuddyai/oczen）的 `ChatStream` 全用带 Timeout 的 HTTP ⇒ 长流式会在超时点被掐断。现全部补上 `StreamHTTP`，并同步修正 main.go **两处** `applyProxies`（启动 + 面板热更新）都传入 `{HTTP, BillingHTTP?, StreamHTTP}`，避免流式漏掉代理。

**回归测试**：`internal/{qwenwork,qodercn,qodercom,qoder}/stream_truncation_test.go` + `internal/upstream.TestStreamTruncationNotDisguisedAsDone`，覆盖四种输入（传输层错误 / 半帧 / 正常 [DONE] / EOF 漏发）。

### issue #50 — TraeWork 登录恒报 400/10101（首因被换 origin 掩盖）

**现象**：浏览器授权完成，但后台用 AuthCode 换 token 恒失败，每 ~2s 刷一条 `400 {"Code":"10101","Message":"无效参数：{__Message.field}."}`（占位符原样返回），直到超时/取消，账号始终加不进去。

**实证根因**：
1. `ExchangeAuthCode` 对 **4xx 也 `continue` 换 origin** 重试，把首选 `api.trae.cn` 的真实错误（403/20401 设备数上限）覆盖成回退 origin `api.trae.com.cn` 的 `400/10101` 后再打印 ⇒ 表象彻底指向「参数写错了」；
2. authCode 一次性，4xx 属终态，旧版却每 2s 重试一次刷屏；
3. **设备号每次登录随机重新生成**：`login_trae.Start` 每次 `randHex(32)`，每点一次「+ TraeWork」都相当于新设备，逼近「同一账号最多 10 台设备」上限。

**修法**：
1. **4xx 立即终态**：`ExchangeAuthCode` 对 4xx（除 429/408）返回携带**首个** origin 真实响应的 `*AuthCodeRejectedError`（含 origin/status/body），不再换 origin 掩盖首因；429/408/5xx/传输层错误仍视为暂时性继续重试。
2. **单表识别 20401**：`IsDeviceLimitReached` 命中 `403 + 20401 / device limit` → 可行动提示「去其它设备登出释放名额（同一账号最多 10 台）」。
3. **终态固化 + 轮询短路**：`login_trae.Poll` 把终态错误写入 `login-state.json` 的 `Err` 并清空 AuthCode，后续轮询直接短路，不再每 2s 重消耗同一次性 authCode 刷屏。
4. **设备号持久化**：复用并落盘 `device-id.json`（`login-state.json` 同目录、独立文件、不复用一次性 state），官方客户端本就持久化设备号。

**暂未做**：版本常量对齐 0.1.69（issue 自标「未证实是成因」，行为侧有风险，无证据不动）。

### issue #53 — WorkBuddyAI 模型级 429 拖垮整账号（最小修复）

**现象**：A 的 `deepseek-v4.1-flash` 撞上游每日上限后，A 上还能用的 `gpt-5.6-luna` 也跟着不能用。

**根因**：上游按**模型**限流（6004 / body 含 *switch to the other models*），但 `Classify` 把它归进账号级 `ErrSoftRate` → 冷却整个账号，其它模型一起被拖进 60s 软冷却并被粘性路由跳过。

**修法（最小、低风险，不做大架构改动）**：`workbuddyai.Classify` 对含该标记的 429 判为 `ErrPassthrough`（请求级透传、不冷却账号），透传原文让客户端按 Retry-After 自退避或换模型——与 R16 单账号渠道同哲学。
**明确不做**：pool 升级到 model 维度、`stickyKey` 改为 `(kind,model)`、state v4——那是动 pool/state/handler 三核心、约 370 行的大改造（issue 作者已主动提出可拆两个小 PR，需单独评估立项）。

---

## ⚠️ 已知边界

- `MB_DEFAULT_DESKTOP_ONLY` 的移除：双屏定位本机无法实测（项目单显示器环境）；如需固定主显示器，正确做法是给 `MessageBoxW` 传 owner HWND（超出本版范围）。
- TraeWork 20401 的**确切成因**仍是 issue 里未证实项（同一账号装官方客户端能登录成功）——本版修复了「首因透出」与「设备号持久化」两个确定性正确点，真实错误码能否解现场需在修复后版本复现确认。
- 模型级限流判据（`switch to the other models`）**仅对 WorkBuddyAI**生效；其它渠道的 429 语义仍是账号级。

## 📎 升级提示

- 本版无需迁移 state（不动 `state-*.json` 格式）。
- 流式超时推广后，长回答不再因 `Client.Timeout` 被从中间掐断；若你的 `config.json` 里 `upstream.timeout_seconds` 调得很小，流式场景不再受限于它（非流式一问一答仍受保护）。